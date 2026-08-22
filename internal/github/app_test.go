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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// A GitHub App proves who it is by signing a JWT with its private key, trades
// that for an installation token, and uses the token for everything else. Each
// step is a place the whole thing can silently stop working an hour after it
// was set up, which is what these tests are about.

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, encoded
}

// fakeApp is GitHub's side of the app flow: it checks the assertion, hands out
// installation tokens, and refuses anything presented with the wrong one.
type fakeApp struct {
	t          *testing.T
	key        *rsa.PrivateKey
	mu         sync.Mutex
	requests   []string
	assertions []string
	minted     int
	expiresIn  time.Duration
	installed  bool
}

func (f *fakeApp) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			f.recordAssertion(bearer)
			if !f.installed {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			w.Write([]byte(`{"id":42}`))

		case strings.Contains(r.URL.Path, "/access_tokens"):
			f.recordAssertion(bearer)
			f.mu.Lock()
			f.minted++
			token := "ghs_installation_" + string(rune('a'+f.minted-1))
			expiry := time.Now().Add(f.expiresIn)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"token":      token,
				"expires_at": expiry.Format(time.RFC3339),
			})

		default:
			// Everything else must arrive with an installation token, never
			// with the JWT: the app's own assertion cannot do this work.
			if !strings.HasPrefix(bearer, "ghs_installation_") {
				f.t.Errorf("%s was called with %q, want an installation token", r.URL.Path, bearer)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if strings.HasSuffix(r.URL.Path, "registration-token") {
				w.Write([]byte(`{"token":"AAAA-registration"}`))
				return
			}
			w.Write([]byte(`{"runners":[{"id":1,"name":"web-1","status":"online","busy":true}]}`))
		}
	})
}

// recordAssertion checks the JWT is one this app could have signed, and keeps
// it for the tests that look at its contents.
func (f *fakeApp) recordAssertion(assertion string) {
	f.t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		f.t.Errorf("want a three-part JWT, got %q", assertion)
		return
	}
	signed := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Errorf("the signature is not base64url: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		f.t.Errorf("the assertion is not signed by the app's key: %v", err)
	}

	f.mu.Lock()
	f.assertions = append(f.assertions, assertion)
	f.mu.Unlock()
}

func (f *fakeApp) claims(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(f.assertions[i], ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func (f *fakeApp) header(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(f.assertions[i], ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatal(err)
	}
	return header
}

func newAppClient(t *testing.T, installationID int64) (*Client, *fakeApp) {
	t.Helper()
	key, encoded := testKey(t)
	fake := &fakeApp{t: t, key: key, expiresIn: time.Hour, installed: true}
	srv := newServer(t, fake.handler())

	client, err := NewApp(123456, encoded, installationID, WithBaseURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	return client, fake
}

func newServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAppMintsAnInstallationTokenAndUsesIt(t *testing.T) {
	client, fake := newAppClient(t, 0)

	token, err := client.RegistrationToken(context.Background(), repoScope())
	if err != nil {
		t.Fatal(err)
	}
	if token != "AAAA-registration" {
		t.Fatalf("got %q", token)
	}

	// The order matters: find the installation, buy a token, then do the work.
	want := []string{
		"GET /repos/clems4ever/runyard/installation",
		"POST /app/installations/42/access_tokens",
		"POST /repos/clems4ever/runyard/actions/runners/registration-token",
	}
	fake.mu.Lock()
	got := append([]string(nil), fake.requests...)
	fake.mu.Unlock()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls were:\n%v\nwant:\n%v", got, want)
	}
}

func TestAppAssertionIsWellFormed(t *testing.T) {
	client, fake := newAppClient(t, 42)
	if _, err := client.RegistrationToken(context.Background(), repoScope()); err != nil {
		t.Fatal(err)
	}

	if header := fake.header(t, 0); header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("header is %v", header)
	}

	claims := fake.claims(t, 0)
	// The issuer is the app id, as a string: GitHub rejects a number here.
	if claims["iss"] != "123456" {
		t.Fatalf("iss is %v (%T), want the app id as a string", claims["iss"], claims["iss"])
	}

	iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64))
	now := time.Now().Unix()
	// Backdated, because GitHub refuses a token issued in the future and a
	// host's clock is often a second or two fast.
	if iat > now {
		t.Errorf("iat is in the future by %ds", iat-now)
	}
	if now-iat > 120 {
		t.Errorf("iat is %ds in the past, further than clock skew explains", now-iat)
	}
	// Inside GitHub's ten-minute ceiling.
	if exp-iat > 600 {
		t.Errorf("the assertion lives %ds, and GitHub's limit is 600", exp-iat)
	}
	if exp <= now {
		t.Error("the assertion is already expired when it is sent")
	}
}

// The token is worth caching: a reconcile pass makes several calls, and every
// one of them buying a new token would be three requests instead of one.
func TestAppReusesAnInstallationTokenUntilItIsNearlySpent(t *testing.T) {
	client, fake := newAppClient(t, 42)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := client.Runners(ctx, repoScope()); err != nil {
			t.Fatal(err)
		}
	}
	if fake.minted != 1 {
		t.Fatalf("minted %d tokens for five calls", fake.minted)
	}
}

func TestAppRefreshesATokenBeforeItExpires(t *testing.T) {
	client, fake := newAppClient(t, 42)
	ctx := context.Background()

	if _, err := client.Runners(ctx, repoScope()); err != nil {
		t.Fatal(err)
	}
	if fake.minted != 1 {
		t.Fatalf("minted %d", fake.minted)
	}

	// Wind the client's clock to inside the refresh window. A registration
	// that starts on a token with a minute left must not fail halfway.
	client.app.now = func() time.Time { return time.Now().Add(time.Hour - refreshBefore) }

	if _, err := client.Runners(ctx, repoScope()); err != nil {
		t.Fatal(err)
	}
	if fake.minted != 2 {
		t.Fatalf("minted %d, want a fresh token once the old one was nearly spent", fake.minted)
	}
}

// One app can be installed on several accounts, and a token is only good for
// the installation it was minted for.
func TestAppKeepsATokenPerScope(t *testing.T) {
	client, fake := newAppClient(t, 42)
	ctx := context.Background()

	if _, err := client.Runners(ctx, repoScope()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Runners(ctx, orgScope()); err != nil {
		t.Fatal(err)
	}
	if fake.minted != 2 {
		t.Fatalf("minted %d tokens for two scopes", fake.minted)
	}
}

func TestAppFindsItsOwnInstallation(t *testing.T) {
	client, fake := newAppClient(t, 0)
	if _, err := client.Runners(context.Background(), orgScope()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.requests[0] != "GET /orgs/runyard-ai/installation" {
		t.Fatalf("looked it up with %q", fake.requests[0])
	}
}

func TestAppSkipsDiscoveryWhenToldTheInstallation(t *testing.T) {
	client, fake := newAppClient(t, 42)
	if _, err := client.Runners(context.Background(), repoScope()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, request := range fake.requests {
		if strings.HasSuffix(request, "/installation") {
			t.Fatalf("it looked up an installation it was given: %v", fake.requests)
		}
	}
}

// The most likely misconfiguration: the app exists, the key is right, and
// nobody installed it on the repository.
func TestAppNotInstalledSaysSo(t *testing.T) {
	key, encoded := testKey(t)
	fake := &fakeApp{t: t, key: key, expiresIn: time.Hour, installed: false}
	client, err := NewApp(123456, encoded, 0, WithBaseURL(newServer(t, fake.handler())))
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Runners(context.Background(), repoScope())
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "find where app 123456 is installed") {
		t.Fatalf("got %q", err)
	}
	// The thing to go and do, rather than advice written for a token: there is
	// no token here, so "check the token's resource owner" helps nobody.
	for _, want := range []string{"no installation covering", "settings/installations"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the advice does not mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "resource owner") {
		t.Errorf("an app was given advice about a token: %v", err)
	}
}

func TestParsePrivateKey(t *testing.T) {
	key, pkcs1 := testKey(t)

	if _, err := ParsePrivateKey(pkcs1); err != nil {
		t.Fatalf("the format GitHub issues was rejected: %v", err)
	}

	// Some tooling converts the key on the way past; both are the same key.
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})
	if _, err := ParsePrivateKey(pkcs8); err != nil {
		t.Fatalf("a PKCS#8 key was rejected: %v", err)
	}
}

// A paste that went wrong has to be caught where the person who pasted it is
// still looking, not at the first runner boot an hour later.
func TestParsePrivateKeyRejectsWhatIsNotOne(t *testing.T) {
	tests := map[string]string{
		"empty":         "",
		"a token":       "github_pat_11ABCDEF",
		"a public key":  "-----BEGIN PUBLIC KEY-----\nMIIBIjAN\n-----END PUBLIC KEY-----\n",
		"truncated":     "-----BEGIN RSA PRIVATE KEY-----\nnot base64\n-----END RSA PRIVATE KEY-----\n",
		"a certificate": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePrivateKey([]byte(content)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestNewAppRejectsAMissingAppID(t *testing.T) {
	_, encoded := testKey(t)
	_, err := NewApp(0, encoded, 0)
	if err == nil || !strings.Contains(err.Error(), "app id") {
		t.Fatalf("got %v", err)
	}
}

func TestNewFromSecret(t *testing.T) {
	_, encoded := testKey(t)

	app, err := NewFromSecret(Secret{IsAppCredential: true, Token: string(encoded), AppID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if app.app == nil {
		t.Fatal("an app credential built a token client")
	}

	pat, err := NewFromSecret(Secret{Token: "github_pat_11ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if pat.app != nil || pat.token != "github_pat_11ABC" {
		t.Fatal("a token credential built an app client")
	}
}

// A clock that is wrong is the failure people spend an afternoon on, so the
// 401 has to mention it.
func TestUnauthorizedMentionsTheAppSpecificCauses(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})
	_, err := client.RegistrationToken(context.Background(), repoScope())
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"private key belongs to the app id", "clock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the advice does not mention %q: %v", want, err)
		}
	}
}

var _ = model.CredentialApp
