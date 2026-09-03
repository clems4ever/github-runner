package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/clems4ever/github-runner/internal/model"
)

// DefaultBranchFile reads a file from a repository's default branch, or
// reports that there is none.
//
// It takes no ref, and that is the security property rather than an omission.
// The file this is used for describes an image the host builds and runs as
// root, so the branch it is read from decides who can change what runs on the
// host. GitHub's contents API defaults to the default branch when no ref is
// given, so a caller cannot ask for a pull request's head — there is no
// argument for it to pass. An edit has to be merged, by whoever is allowed to
// merge, before it is worth anything.
//
// A missing file is (nil, nil): most repositories will never have one, and
// asking every repository in a pool about a file that is usually absent must
// not look like a failure.
func (c *Client) DefaultBranchFile(ctx context.Context, scope Scope, path string) ([]byte, error) {
	if scope.Kind != model.ScopeRepository {
		return nil, fmt.Errorf("%s is an organisation, and a file belongs to a repository", scope.Path)
	}

	owner, repo, ok := strings.Cut(scope.Path, "/")
	if !ok {
		return nil, fmt.Errorf("%q is not owner/repository", scope.Path)
	}

	var out struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int64  `json:"size"`
	}
	// Each path segment is escaped on its own: the separators are part of the
	// URL and the names are not.
	var segments []string
	for _, segment := range strings.Split(path, "/") {
		segments = append(segments, url.PathEscape(segment))
	}
	api := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/contents/" + strings.Join(segments, "/")

	if err := c.do(ctx, http.MethodGet, api, &out, scope); err != nil {
		var refused *Error
		if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}

	if out.Type != "file" {
		// A directory, a submodule or a symlink. None of them is a definition,
		// and following the symlink would be reading a path this did not ask
		// for.
		return nil, fmt.Errorf("%s is a %s, not a file", path, out.Type)
	}
	if out.Encoding != "base64" {
		// Over a megabyte, GitHub sends the metadata and leaves the content
		// out. Nothing this reads is anywhere near that, so a file that hits it
		// is not a definition somebody wrote by hand.
		return nil, fmt.Errorf("%s is %d bytes, which is too large to read this way", path, out.Size)
	}

	// GitHub wraps the base64 at 60 columns, which Go's decoder will not take.
	decoded, err := base64.StdEncoding.DecodeString(
		strings.NewReplacer("\n", "", "\r", "").Replace(out.Content))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return decoded, nil
}
