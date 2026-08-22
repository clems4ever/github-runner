package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// A GitHub App does not have a token. It has a private key, which is used to
// sign a short JWT proving "I am this app", which is exchanged for an
// installation token that expires in an hour and can actually do things.
//
// That indirection is the point. Nothing here expires on a calendar the way a
// personal access token does, the repositories reachable are a list the owner
// edits without touching the credential, and uninstalling the app revokes
// everything at once.
const (
	// jwtLifetime is well inside GitHub's ten-minute ceiling. A JWT is only
	// used to fetch an installation token, so a short one costs nothing.
	jwtLifetime = 9 * time.Minute
	// jwtBackdate covers a host whose clock runs slightly fast; GitHub rejects
	// a token issued in the future outright.
	jwtBackdate = 60 * time.Second
	// refreshBefore is how long before expiry an installation token is
	// replaced. A registration that starts at the last second must not fail
	// halfway through.
	refreshBefore = 5 * time.Minute
)

// appAuth turns an app's private key into installation tokens, and remembers
// them until they are nearly expired.
type appAuth struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	now            func() time.Time

	mu     sync.Mutex
	tokens map[string]installationToken // keyed by scope
}

type installationToken struct {
	token   string
	expires time.Time
}

// ParsePrivateKey reads an app's PEM private key.
//
// GitHub hands out PKCS#1 ("BEGIN RSA PRIVATE KEY"); some tooling converts it
// to PKCS#8 on the way past, so both are accepted. Anything else is rejected
// here, where whoever pasted it is still looking, rather than at the first
// runner boot an hour later.
func ParsePrivateKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("this is not a PEM file: it should begin with -----BEGIN RSA PRIVATE KEY-----")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("this PEM is not a private key GitHub would have issued: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the key is %T, and a GitHub App's key is RSA", parsed)
	}
	return key, nil
}

// NewApp builds a client that authenticates as a GitHub App installation.
//
// An installation id of zero means "work it out": an app installed on one
// account has one installation, and looking its id up is a step with no
// decision in it.
func NewApp(appID int64, privateKey []byte, installationID int64, opts ...Option) (*Client, error) {
	if appID <= 0 {
		return nil, errors.New("an app id is required; it is on the app's settings page")
	}
	key, err := ParsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	client := New("", opts...)
	client.app = &appAuth{
		appID:          appID,
		installationID: installationID,
		key:            key,
		now:            time.Now,
		tokens:         map[string]installationToken{},
	}
	return client, nil
}

// NewFromSecret builds whichever kind of client a credential calls for, so
// nothing above this package has to know there are two.
func NewFromSecret(secret Secret, opts ...Option) (*Client, error) {
	if secret.IsApp() {
		return NewApp(secret.AppID, []byte(secret.Token), secret.InstallationID, opts...)
	}
	return New(secret.Token, opts...), nil
}

// Secret mirrors model.Secret, kept here so this package does not depend on
// the model for one struct.
type Secret struct {
	IsAppCredential bool
	Token           string
	AppID           int64
	InstallationID  int64
}

// IsApp reports whether this credential is a GitHub App.
func (s Secret) IsApp() bool { return s.IsAppCredential }

// bearer returns the token to authenticate one call to one scope.
func (a *appAuth) bearer(ctx context.Context, c *Client, scope Scope) (string, error) {
	key := string(scope.Kind) + ":" + scope.Path

	a.mu.Lock()
	cached, ok := a.tokens[key]
	a.mu.Unlock()
	if ok && a.now().Before(cached.expires.Add(-refreshBefore)) {
		return cached.token, nil
	}

	assertion, err := a.signJWT()
	if err != nil {
		return "", err
	}

	installation := a.installationID
	if installation == 0 {
		installation, err = a.discoverInstallation(ctx, c, assertion, scope)
		if err != nil {
			return "", err
		}
	}

	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installation)
	if err := c.doAs(ctx, http.MethodPost, path, assertion, &minted, scope); err != nil {
		return "", fmt.Errorf("exchange the app's key for an installation token: %w", err)
	}
	if minted.Token == "" {
		return "", errors.New("GitHub returned no installation token")
	}
	if minted.ExpiresAt.IsZero() {
		// GitHub always sends one; assume the documented hour if it ever does
		// not, rather than treating the token as immortal.
		minted.ExpiresAt = a.now().Add(time.Hour)
	}

	a.mu.Lock()
	a.tokens[key] = installationToken{token: minted.Token, expires: minted.ExpiresAt}
	a.mu.Unlock()
	return minted.Token, nil
}

// discoverInstallation asks which installation covers this scope.
func (a *appAuth) discoverInstallation(ctx context.Context, c *Client, assertion string, scope Scope) (int64, error) {
	var installation struct {
		ID int64 `json:"id"`
	}
	if err := c.doAs(ctx, http.MethodGet, scope.prefix()+"/installation", assertion, &installation, scope); err != nil {
		return 0, fmt.Errorf("find where app %d is installed for %s: %w", a.appID, scope.Path, err)
	}
	if installation.ID == 0 {
		return 0, fmt.Errorf("app %d is not installed on %s", a.appID, scope.Path)
	}
	return installation.ID, nil
}

// grantURL is the page where someone can give this app access to a repository.
//
// The app's own installations are readable with its assertion even when the
// repository in question is not, which is what makes this possible at the exact
// moment it is needed. One installation is the ordinary case for an app
// installed on one account, and its page has the repository picker on it;
// anything else falls back to the list.
func (a *appAuth) grantURL(ctx context.Context, c *Client) string {
	const fallback = "https://github.com/settings/installations"

	assertion, err := a.signJWT()
	if err != nil {
		return fallback
	}

	var installations []struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := c.doAs(ctx, http.MethodGet, "/app/installations", assertion, &installations, Scope{}); err != nil {
		return fallback
	}
	if len(installations) == 1 && installations[0].HTMLURL != "" {
		return installations[0].HTMLURL
	}
	return fallback
}

// signJWT builds the assertion that proves ownership of the app's key.
func (a *appAuth) signJWT() (string, error) {
	now := a.now()
	header := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := base64URL([]byte(fmt.Sprintf(
		`{"iat":%d,"exp":%d,"iss":"%s"}`,
		now.Add(-jwtBackdate).Unix(),
		now.Add(jwtLifetime).Unix(),
		strconv.FormatInt(a.appID, 10),
	)))

	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign the app assertion: %w", err)
	}
	return signing + "." + base64URL(signature), nil
}

// base64URL is the unpadded URL-safe encoding a JWT is made of.
func base64URL(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// jsonError is here so app.go can decode an error body the same way do() does.
var _ = json.Unmarshal
