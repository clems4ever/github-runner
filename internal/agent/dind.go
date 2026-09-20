package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// dockerSocket is where the daemon started inside a container listens, and
// where the docker client in a job looks without being told anything.
const dockerSocket = "/var/run/docker.sock"

// dockerStartTimeout is how long the daemon is given to answer. It is a local
// process on a filesystem that is already there; a daemon that has not
// answered in this long is not slow, it has failed, and the reason is in the
// console above the error.
const dockerStartTimeout = 60 * time.Second

// dockerStopGrace is how long dockerd is given to shut down after the runner
// has finished. It only has to stop its own containers, which the job left
// behind and nothing is waiting for.
const dockerStopGrace = 20 * time.Second

// account is the unprivileged user the runner itself runs as.
//
// A container with a daemon in it starts as root, because nothing else can
// start one — but the job must not inherit that. The runner is put back on the
// account the image built it for, which is found by looking at who owns the
// runner rather than by assuming a name: the official image uses runner:docker
// at 1001:123, and an image somebody built themselves uses whatever they chose.
type account struct {
	uid, gid uint32
	// switching is false when the agent is already the right user, which is
	// every container without a daemon in it: the image's own USER is in
	// effect and there is nothing to drop to.
	switching bool
}

func runnerAccount(home string) (account, error) {
	info, err := os.Stat(home)
	if err != nil {
		return account{}, fmt.Errorf("who owns %s: %w", home, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return account{}, fmt.Errorf("who owns %s: the filesystem did not say", home)
	}
	return account{
		uid: stat.Uid,
		gid: stat.Gid,
		// Nothing to do when the agent is not root, and nothing sensible to do
		// when the runner is owned by root as well — an image that ships it
		// that way has already decided, and refusing to start would be worse
		// than honouring it.
		switching: os.Geteuid() == 0 && stat.Uid != 0,
	}, nil
}

// apply puts a command on the runner's account.
//
// HOME is replaced rather than added: the agent's own is root's, the child
// inherits the whole environment, and a runner writing its credentials and its
// work directory into /root is a runner that works until the first job that
// looks for anything in the home directory it said it had.
func (a account) apply(cmd *exec.Cmd, home string) {
	if !a.switching {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: a.uid, Gid: a.gid},
	}
	cmd.Env = withEnv(os.Environ(), "HOME", home)
}

// withEnv sets one variable in an environment, replacing what was there.
func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok && name == key {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+value)
}

// startDocker runs a Docker daemon inside this container and waits for it to
// answer, returning the function that stops it again.
//
// The daemon is the runner's, not the host's: its images, its build cache and
// the containers a job starts all live inside this container and go when it
// does. That is the whole point of the arrangement — a job gets docker without
// the host's socket being handed to it — and it is also its cost, since
// nothing is shared between two runners and every pool pays for its own pulls.
func startDocker(ctx context.Context, acct account, log *slog.Logger) (stop func(), err error) {
	if err := dockerPresent(); err != nil {
		return nil, err
	}

	// Before the daemon, and it is not optional on a cgroup v2 host: see
	// nestCgroups.
	if err := nestCgroups(log); err != nil {
		return nil, err
	}

	log.Info("starting the docker daemon inside this runner")
	daemon := exec.Command("dockerd",
		"--host=unix://"+dockerSocket,
		// Quiet on purpose: this shares a console with the runner, and the
		// thing somebody reads it for is the job.
		"--log-level=warn",
	)
	daemon.Stdout, daemon.Stderr = os.Stdout, os.Stderr
	// Its own process group, so that a signal sent to the agent's group — a
	// docker stop reaches the whole container — does not race the orderly
	// shutdown below.
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := daemon.Start(); err != nil {
		return nil, fmt.Errorf("start dockerd: %w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()

	stop = func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(dockerStopGrace):
			_ = daemon.Process.Kill()
			<-exited
		}
	}

	if err := waitForDocker(ctx, exited); err != nil {
		stop()
		return nil, err
	}

	// The runner is not root, and dockerd makes its socket root-owned unless
	// it is told about a group. Told here rather than with --group, because
	// the group a runner belongs to is the image's decision and this is the
	// one thing that is true of every image: whoever owns the runner is who
	// has to be able to reach the daemon.
	if acct.switching {
		if err := os.Chown(dockerSocket, -1, int(acct.gid)); err != nil {
			stop()
			return nil, fmt.Errorf("hand the docker socket to the runner's group: %w", err)
		}
		if err := os.Chmod(dockerSocket, 0o660); err != nil {
			stop()
			return nil, fmt.Errorf("hand the docker socket to the runner's group: %w", err)
		}
	}

	log.Info("docker is ready")
	return stop, nil
}

// cgroupRoot is where this container's cgroup namespace is mounted. A variable
// so a test can point it at a directory it made.
var cgroupRoot = "/sys/fs/cgroup"

// nestCgroups makes this container's cgroup namespace one that a daemon inside
// it can create containers in.
//
// Cgroup v2 has a rule with no exceptions: a cgroup may hold processes, or it
// may have controllers enabled for its children, but not both. A container's
// namespace root starts out holding every process in the container — the
// agent, the runner, the job — so the moment the daemon inside asks for a child
// cgroup with `cpu` or `memory` on it, the kernel refuses. What it says is:
//
//	unable to apply cgroup configuration: cannot enter cgroupv2
//	"/sys/fs/cgroup/docker" with domain controllers -- it is in threaded mode
//
// which names a cgroup somebody would have to already understand to act on,
// and arrives as a container that will not start rather than as a daemon that
// will not run. The daemon is fine. Everything it starts fails.
//
// The fix is the one the official dind image has had for years: move what is in
// the root into a leaf of its own, then delegate the controllers to children.
// The root then holds no processes and may enable anything; the daemon's
// containers get their own cgroups underneath it, which is where a pool's
// memory and processor share is enforced.
//
// It is a no-op on cgroup v1, and on a root that has already been nested — a
// runner restarted in place, or a host whose runtime did it first.
func nestCgroups(log *slog.Logger) error {
	controllers, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		// No cgroup.controllers is cgroup v1, where none of this applies and
		// the daemon has always worked.
		return nil
	}

	procs, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("read this container's cgroup: %w", err)
	}
	pids := strings.Fields(string(procs))
	if len(pids) > 0 {
		leaf := filepath.Join(cgroupRoot, "init")
		if err := os.MkdirAll(leaf, 0o755); err != nil {
			return fmt.Errorf("make the cgroup the container's own processes move into: %w", err)
		}
		// One at a time, because cgroup.procs takes one pid per write and
		// reports the first refusal by failing the write.
		//
		// A pid that has gone between the read and the write is not an error
		// here: it is a process that exited, and the only thing that matters is
		// that the root ends up empty.
		moved := 0
		for _, pid := range pids {
			if err := appendTo(filepath.Join(leaf, "cgroup.procs"), pid); err == nil {
				moved++
			}
		}
		log.Info("moved this container's processes out of its cgroup root",
			"processes", moved, "of", len(pids))
	}

	// Delegate every controller this namespace has to its children. Written as
	// one line because the kernel applies it as one: a partial write leaves the
	// ones before it enabled, which is a state nothing here would know to
	// unpick.
	var enable strings.Builder
	for _, controller := range strings.Fields(string(controllers)) {
		if enable.Len() > 0 {
			enable.WriteByte(' ')
		}
		enable.WriteString("+" + controller)
	}
	if enable.Len() == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"),
		[]byte(enable.String()), 0o644); err != nil {
		return fmt.Errorf("delegate %s to this container's children, which is what "+
			"lets the daemon inside it start containers: %w", enable.String(), err)
	}
	return nil
}

// appendTo writes one line to a kernel file that takes one write at a time.
func appendTo(path, line string) error {
	// O_CREATE because the kernel makes `cgroup.procs` when the directory is
	// made and a test's fake directory starts empty. Not O_TRUNC: cgroupfs
	// ignores truncation, a fake does not, and a helper that only works
	// against the kernel is one nothing can check.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

// dockerPresent reports whether this image can run a daemon at all, in terms
// that say what to do about it.
//
// Checked before starting rather than after failing: dockerd without iptables
// dies several seconds in, inside a message about a network controller, and
// the pool then looks like one whose runners keep restarting for no reason.
// The stock runner image is the one people hit — it carries the whole static
// Docker bundle, daemon included, on a base with no iptables.
func dockerPresent() error {
	var missing []string
	for _, binary := range []string{"dockerd", "iptables"} {
		if _, err := exec.LookPath(binary); err != nil {
			missing = append(missing, binary)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("this pool asks for docker in docker and its image has no %s. "+
		"The stock runner image carries the docker client and the daemon binaries but not what the "+
		"daemon needs to build a network; point the pool at an image that adds them — images/dind/Dockerfile "+
		"in runner-fleet is one",
		strings.Join(missing, " or "))
}

// waitForDocker blocks until the daemon answers a ping, the daemon exits, or
// the wait runs out.
func waitForDocker(ctx context.Context, exited <-chan error) error {
	ctx, cancel := context.WithTimeout(ctx, dockerStartTimeout)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocket)
			},
		},
		Timeout: 2 * time.Second,
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case err := <-exited:
			// The console above this has the reason. Saying "it exited" and
			// stopping is better than polling a socket that will never appear
			// until the timeout, with the explanation scrolled off.
			return fmt.Errorf("the docker daemon stopped while starting up: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("the docker daemon did not answer within %s", dockerStartTimeout)
		case <-ticker.C:
		}
	}
}
