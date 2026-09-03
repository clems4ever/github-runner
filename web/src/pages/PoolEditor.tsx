import { useState } from "react";
import {
  Alert,
  Badge,
  Button,
  Divider,
  Group,
  NumberInput,
  Select,
  SegmentedControl,
  SimpleGrid,
  Stack,
  Switch,
  TagsInput,
  Text,
  Textarea,
  TextInput,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { notifications } from "@mantine/notifications";
import { IconAlertTriangle, IconExternalLink } from "@tabler/icons-react";
import {
  api,
  ApiError,
  effectiveLabels,
  type Credential,
  type Pool,
} from "../api";

/**
 * The pool editor.
 *
 * Everything that decides what a runner is lives on one screen, because the
 * combinations matter to each other: a container with nested virtualisation is
 * a different security proposition from a machine with it, and the labels a
 * workflow will target follow from both.
 */
/**
 * Whether this pool could read a definition from the repository it serves.
 *
 * The same rule the daemon enforces: a layer is an overlay on a machine's disk,
 * and it belongs to one repository. A container has no disk of that shape, and
 * an organisation pool has no one repository.
 */
function layersPossible(pool: Partial<Pool>): boolean {
  return pool.runtime === "vm" && pool.scopeKind === "repository";
}

/**
 * Whether this pool could sleep.
 *
 * The daemon's rule: a sleeping pool is woken by reading the queue of the
 * repository it serves, and GitHub lists queued jobs per repository. An
 * organisation has no queue to read, so nothing would ever wake it — and a
 * pool at zero that nothing wakes is a pool that has quietly stopped.
 */
function sleepPossible(pool: Partial<Pool>): boolean {
  return pool.scopeKind === "repository";
}

/** A refusal from GitHub that a person can act on. */
interface Refusal {
  message: string;
  grantUrl?: string;
}

export function PoolEditor({
  pool,
  credentials,
  onSaved,
  onCancel,
}: {
  pool: Partial<Pool>;
  credentials: Credential[];
  onSaved: () => Promise<void>;
  onCancel: () => void;
}) {
  const [saving, setSaving] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const form = useForm<Partial<Pool>>({
    initialValues: { ...pool },
    validate: {
      name: (value) =>
        /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(value ?? "")
          ? null
          : "Lower-case letters, digits and dashes, not starting or ending with a dash",
      scope: (value, values) => {
        if (!value) return "Required";
        if (
          values.scopeKind === "repository" &&
          !/^[^/]+\/[^/]+$/.test(value)
        ) {
          return "A repository is owner/name";
        }
        if (values.scopeKind === "organization" && value.includes("/")) {
          return "An organisation is a single name";
        }
        return null;
      },
      // One, not zero. A pool with no runners has nothing able to accept a
      // job, so it could never discover that it needs to grow — unless it
      // sleeps, and something is reading the queue on its behalf.
      minReplicas: (value, values) => {
        const floor = values.sleeps ? 0 : 1;
        if ((value ?? 0) >= floor && (value ?? 0) <= 64) return null;
        return values.sleeps
          ? "0 or more"
          : "At least 1 — use the enabled switch to stop a pool entirely";
      },
      // At least one either way: a sleeping pool with a ceiling of zero has
      // nowhere to wake up to.
      maxReplicas: (value, values) =>
        (value ?? 0) >= Math.max(values.minReplicas ?? 1, 1) &&
        (value ?? 0) <= 64
          ? null
          : "At least the minimum, and no more than 64",
    },
  });

  const values = form.values;
  const isVM = values.runtime === "vm";

  return (
    <form
      onSubmit={form.onSubmit(async (submitted) => {
        setSaving(true);
        setRefusal(null);
        // A pool that has been switched to a container, or to an organisation,
        // cannot have layers — the daemon refuses to save one that says it
        // does. The control is hidden by then, so the operator would be
        // reading a refusal about a field they cannot see.
        if (!layersPossible(submitted))
          submitted = { ...submitted, layers: "off" };
        // The same for sleeping, and for the same reason: switched to an
        // organisation, the control is gone and the daemon would refuse to
        // save a field the operator can no longer see.
        if (!sleepPossible(submitted))
          submitted = {
            ...submitted,
            sleeps: false,
            minReplicas: Math.max(submitted.minReplicas ?? 1, 1),
          };
        try {
          if (pool.id) {
            await api.updatePool(pool.id, submitted);
          } else {
            await api.createPool(submitted);
          }
          await onSaved();
        } catch (error) {
          // Shown on the form rather than in a corner toast: this is about a
          // field on this screen, and it usually needs reading twice.
          if (error instanceof ApiError) {
            setRefusal({ message: error.message, grantUrl: error.grantUrl });
          } else {
            notifications.show({
              color: "red",
              title: "Could not save the pool",
              message: error instanceof Error ? error.message : String(error),
            });
          }
        } finally {
          setSaving(false);
        }
      })}
    >
      <Stack gap="md">
        {/* Pairs of fields side by side are two half-width fields on a phone,
            which is worse than one after the other. */}
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md" verticalSpacing="md">
          <TextInput
            label="Name"
            description="Runners are named after it: web-1, web-2"
            placeholder="web"
            withAsterisk
            {...form.getInputProps("name")}
          />
          <Select
            label="Credential"
            description="Mints a registration token per start"
            withAsterisk
            data={credentials.map((c) => ({
              value: String(c.id),
              label: `${c.name} (${c.hint})`,
            }))}
            value={values.credentialId ? String(values.credentialId) : null}
            onChange={(value) =>
              form.setFieldValue("credentialId", Number(value))
            }
          />
        </SimpleGrid>

        <div>
          <Text size="sm" fw={500} mb={4}>
            Scope
          </Text>
          <SimpleGrid
            cols={{ base: 1, sm: 2 }}
            spacing="sm"
            verticalSpacing="sm"
          >
            <SegmentedControl
              fullWidth
              data={[
                { value: "repository", label: "Repository" },
                { value: "organization", label: "Organisation" },
              ]}
              value={values.scopeKind}
              onChange={(value) =>
                form.setFieldValue("scopeKind", value as Pool["scopeKind"])
              }
            />
            <TextInput
              placeholder={
                values.scopeKind === "organization"
                  ? "my-org"
                  : "owner/repository"
              }
              {...form.getInputProps("scope")}
            />
          </SimpleGrid>
          {values.scopeKind === "organization" && (
            <Text size="xs" c="dimmed" mt={4}>
              Every repository in the organisation can use these runners. GitHub
              has no equivalent for a personal account.
            </Text>
          )}
        </div>

        <Divider label="Runtime" labelPosition="left" />

        <SegmentedControl
          fullWidth
          data={[
            { value: "vm", label: "Virtual machine" },
            { value: "container", label: "Container" },
          ]}
          value={values.runtime}
          onChange={(value) =>
            form.setFieldValue("runtime", value as Pool["runtime"])
          }
        />
        <Text size="xs" c="dimmed" mt={-8}>
          {isVM
            ? "A machine per job: its own kernel, its own Docker daemon, nothing shared with the host."
            : "A container per runner: faster to start and cheaper to run, but a weaker boundary than a machine."}
        </Text>

        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md" verticalSpacing="sm">
          <Switch
            label="Ephemeral"
            description="Take one job, then be replaced by a clean runner"
            checked={values.ephemeral ?? false}
            onChange={(event) =>
              form.setFieldValue("ephemeral", event.currentTarget.checked)
            }
          />
          <Switch
            label="Nested virtualisation"
            description={
              isVM
                ? "Jobs can boot machines of their own"
                : "Passes the host /dev/kvm in"
            }
            checked={values.nested ?? false}
            onChange={(event) =>
              form.setFieldValue("nested", event.currentTarget.checked)
            }
          />
        </SimpleGrid>

        {values.nested && !isVM && (
          <Alert
            color="orange"
            variant="light"
            icon={<IconAlertTriangle size={16} />}
          >
            A container with nested virtualisation gets the host's KVM device.
            That is a real hole in an already weaker boundary — a virtual
            machine is the safer place for jobs that need it.
          </Alert>
        )}

        <Divider label="Scaling" labelPosition="left" />

        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md" verticalSpacing="md">
          <NumberInput
            label="Minimum runners"
            description="Kept up even when nothing is running"
            min={values.sleeps ? 0 : 1}
            max={64}
            {...form.getInputProps("minReplicas")}
          />
          <NumberInput
            label="Maximum runners"
            description="Set it equal to the minimum for a fixed size"
            min={1}
            max={64}
            {...form.getInputProps("maxReplicas")}
          />
        </SimpleGrid>
        <Text size="xs" c="dimmed" mt={-8}>
          {values.sleeps && (values.minReplicas ?? 0) === 0
            ? `Nothing runs while ${values.scope || "the repository"} is quiet. A job waiting for these labels starts as many runners as it needs, up to ${values.maxReplicas}, and they go again after a few minutes with no work.`
            : (values.maxReplicas ?? 1) > (values.minReplicas ?? 1)
              ? `The pool sits at ${values.minReplicas} and adds a runner whenever every one of them is busy, up to ${values.maxReplicas}. It returns to ${values.minReplicas} after a few minutes with no work.`
              : `A fixed ${values.minReplicas} runner${(values.minReplicas ?? 1) === 1 ? "" : "s"}: it never grows, however much work arrives.`}
        </Text>

        {/*
          The setting the operator came here for after a host filled up: a pool
          that keeps a machine up around the clock to notice work that arrives
          twice a week. It is under Scaling rather than in a section of its own
          because it is the floor, said a different way.
        */}
        {sleepPossible(values) && (
          <Switch
            label="Let this pool sleep"
            description="Keep nothing running while the repository is quiet, and start machines when a job is waiting for them."
            checked={values.sleeps ?? false}
            onChange={(event) => {
              const on = event.currentTarget.checked;
              form.setFieldValue("sleeps", on);
              // The switch is about the floor, so it moves the floor. Leaving
              // a minimum of one behind would be a pool that says it sleeps
              // and never does.
              if (on) form.setFieldValue("minReplicas", 0);
              else if ((values.minReplicas ?? 0) < 1)
                form.setFieldValue("minReplicas", 1);
              if ((values.maxReplicas ?? 0) < 1)
                form.setFieldValue("maxReplicas", 1);
            }}
          />
        )}
        {values.sleeps && (
          <Text size="xs" c="dimmed" mt={-8}>
            The daemon asks GitHub what is queued for this repository once a
            pass while the pool is asleep or fully busy, so waking costs a boot
            — around a minute — on the first job after a quiet spell.
          </Text>
        )}

        <Divider label="Size" labelPosition="left" />

        {/* Three number inputs across a phone leaves no room for the steppers,
            which is where the digits went. */}
        <SimpleGrid cols={{ base: 2, sm: 3 }} spacing="md" verticalSpacing="md">
          <NumberInput
            label="vCPUs"
            min={1}
            max={64}
            {...form.getInputProps("cpus")}
          />
          <NumberInput
            label="Memory (MiB)"
            min={512}
            step={512}
            {...form.getInputProps("memoryMb")}
          />
          {isVM && (
            <NumberInput
              label="Disk (GiB)"
              min={10}
              {...form.getInputProps("diskGb")}
            />
          )}
        </SimpleGrid>
        <Text size="xs" c="dimmed" mt={-8}>
          Every runner is a machine of this size, so at its maximum the pool
          wants {((values.maxReplicas ?? 0) * (values.memoryMb ?? 0)) / 1024}{" "}
          GiB of memory at once.
        </Text>

        <Divider label="Labels" labelPosition="left" />

        <TagsInput
          label="Extra labels"
          description="What a workflow targets with runs-on"
          placeholder="gpu, eu-west"
          value={values.labels ?? []}
          onChange={(value) => form.setFieldValue("labels", value)}
        />
        <div>
          <Text size="xs" c="dimmed" mb={4}>
            Runners will register with:
          </Text>
          <Group gap={4}>
            <Badge size="sm" variant="default">
              self-hosted
            </Badge>
            {effectiveLabels(values).map((label) => (
              <Badge key={label} size="sm" variant="dot">
                {label}
              </Badge>
            ))}
          </Group>
        </div>

        <Divider label="Image" labelPosition="left" />

        <TextInput
          label="Image"
          description={
            isVM
              ? "Names this pool’s image. Two pools that bake the same thing share one; a name of its own gives this pool one of its own."
              : "The container image these runners run. It has to carry the Actions runner."
          }
          {...form.getInputProps("image")}
        />

        {/*
          Machines only. A container does not build an image, it runs one
          somebody else built, and the daemon refuses both of these on a
          container pool rather than ignoring them.
        */}
        {isVM && (
          <>
            <TagsInput
              label="Extra packages"
              description="apt packages baked in, so a job does not install them every time"
              placeholder="nftables, conntrack"
              value={values.packages ?? []}
              onChange={(value) => form.setFieldValue("packages", value)}
            />
            <Textarea
              label="Recipe"
              description="Shell, run as root while the image is built — for what apt cannot give: a pinned toolchain, a linter, a warm build cache. Editing it builds a new image and replaces this pool's runners as they finish."
              placeholder={
                "curl -fsSL https://go.dev/dl/go1.25.0.linux-amd64.tar.gz | tar -C /usr/local -xz"
              }
              rows={10}
              styles={{
                input: { fontFamily: "var(--mantine-font-family-monospace)" },
              }}
              {...form.getInputProps("recipe")}
            />
          </>
        )}

        {/*
          Layers are a repository's own definition, built on top of this pool's
          image. Only a repository-scoped machine pool can have them: an
          organisation pool's runner is built before it knows whose job it will
          take, so there is no one repository whose layer it could have.
        */}
        {layersPossible(values) && (
          <>
            <Divider label="Repository definitions" labelPosition="left" />
            <Select
              label="Let the repository add to this image"
              description={`Read from .github/runner-fleet.yml on ${values.scope || "the repository"}’s default branch.`}
              allowDeselect={false}
              data={[
                {
                  value: "off",
                  label: "No — this pool’s image is the whole of it",
                },
                {
                  value: "approve",
                  label: "Yes, once I have approved each definition",
                },
                { value: "trust", label: "Yes, whatever it asks for, unread" },
              ]}
              value={values.layers ?? "off"}
              onChange={(value) =>
                form.setFieldValue("layers", (value ?? "off") as Pool["layers"])
              }
            />
            {values.layers === "trust" && (
              <Alert
                color="yellow"
                variant="light"
                icon={<IconAlertTriangle size={16} />}
              >
                <Text size="sm">
                  Anyone who can merge to that repository’s default branch can
                  run a script as root on a machine on this host, without anyone
                  reading it first. That is a reasonable thing to allow — it is
                  close to what merging a workflow already buys — but it is
                  worth meaning.
                </Text>
              </Alert>
            )}
            {values.layers === "approve" && (
              <Text size="xs" c="dimmed" mt={-8}>
                Each new definition waits on the Layers page. Until one is
                approved the pool keeps running on the image it has, so a change
                here never takes a repository’s runners away.
              </Text>
            )}
          </>
        )}

        {refusal && (
          <Alert
            color="red"
            variant="light"
            icon={<IconAlertTriangle size={16} />}
            title="GitHub refused this"
          >
            <Text size="sm">{refusal.message}</Text>
            {refusal.grantUrl && (
              <Button
                component="a"
                href={refusal.grantUrl}
                target="_blank"
                rel="noreferrer"
                size="xs"
                variant="light"
                mt="sm"
                rightSection={<IconExternalLink size={14} />}
              >
                Grant access on GitHub
              </Button>
            )}
          </Alert>
        )}

        <Group justify="space-between" mt="sm" gap="sm" wrap="wrap">
          <Switch
            label="Enabled"
            checked={values.enabled ?? true}
            onChange={(event) =>
              form.setFieldValue("enabled", event.currentTarget.checked)
            }
          />
          <Group gap="sm" wrap="nowrap">
            <Button variant="default" onClick={onCancel} type="button">
              Cancel
            </Button>
            <Button type="submit" loading={saving}>
              {pool.id ? "Save" : "Create"}
            </Button>
          </Group>
        </Group>

        {pool.id && (
          <Text size="xs" c="dimmed">
            Changing anything but the scaling bounds replaces the pool's
            runners. They are drained first, so no job is lost — a busy runner
            is replaced when it finishes.
          </Text>
        )}
      </Stack>
    </form>
  );
}
