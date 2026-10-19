package auth_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/auth"
	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

const testClientID = "test-client-id"

// tokenServer is a fake /oauth/token endpoint that rotates refresh tokens the
// way the Rhombus auth server does: each refresh token works once, and
// presenting a spent one is invalid_grant.
type tokenServer struct {
	*httptest.Server
	mu    sync.Mutex
	valid map[string]int // refresh token -> generation it was issued in
	gen   int
	calls atomic.Int32
	delay time.Duration
	forms []map[string]string
	basic []bool
}

func newTokenServer(t *testing.T, refreshToken string) *tokenServer {
	t.Helper()
	ts := &tokenServer{valid: map[string]int{refreshToken: 1}, gen: 1}
	ts.Server = httptest.NewServer(http.HandlerFunc(ts.handle))
	t.Cleanup(ts.Close)
	t.Cleanup(auth.SetTokenURL(ts.URL + "/oauth/token"))
	return ts
}

func (ts *tokenServer) handle(w http.ResponseWriter, r *http.Request) {
	ts.calls.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	_, _, hasBasic := r.BasicAuth()
	form := map[string]string{}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	if ts.delay > 0 {
		time.Sleep(ts.delay)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.forms = append(ts.forms, form)
	ts.basic = append(ts.basic, hasBasic)
	w.Header().Set("Content-Type", "application/json")
	rt := form["refresh_token"]
	if form["grant_type"] != "refresh_token" || form["client_id"] != testClientID {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"invalid_request"}`)
		return
	}
	if _, ok := ts.valid[rt]; !ok {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token reused"}`)
		return
	}
	delete(ts.valid, rt)
	ts.gen++
	next := fmt.Sprintf("refresh-%d", ts.gen)
	ts.valid[next] = ts.gen
	fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":%q,"token_type":"Bearer","expires_in":3600}`, ts.gen, next)
}

// writeOAuthProfile stores an OAuth profile and returns it as loaded from disk.
func writeOAuthProfile(t *testing.T, tok config.OAuthTokens, partner bool) config.Config {
	t.Helper()
	if err := config.SaveOAuthClient(config.DefaultProfile, testClientID, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveOAuthLogin(config.DefaultProfile, tok, partner); err != nil {
		t.Fatal(err)
	}
	return config.LoadConfig(config.DefaultProfile)
}

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RHOMBUS_API_KEY", "")
	t.Setenv("RHOMBUS_PROFILE", "")
	t.Setenv("RHOMBUS_ENDPOINT_URL", "")
	return home
}

func expiresIn(d time.Duration) int64 { return time.Now().Add(d).Unix() }

func TestHeadersPerCredentialKind(t *testing.T) {
	setHome(t)
	far := expiresIn(time.Hour)
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want map[string]string
	}{
		{"token", config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1"},
			map[string]string{"x-auth-scheme": "api-token", "x-auth-apikey": "key-1"}},
		{"partner token", config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", IsPartner: true},
			map[string]string{"x-auth-scheme": "partner-api-token", "x-auth-apikey": "key-1"}},
		{"cert", config.Config{AuthType: config.AuthTypeCert, ApiKey: "key-1", CertFile: "c", KeyFile: "k"},
			map[string]string{"x-auth-scheme": "api", "x-auth-apikey": "key-1"}},
		{"partner cert", config.Config{AuthType: config.AuthTypeCert, ApiKey: "key-1", CertFile: "c", KeyFile: "k", IsPartner: true},
			map[string]string{"x-auth-scheme": "partner-api", "x-auth-apikey": "key-1"}},
		{"incomplete cert profile uses the token scheme", config.Config{AuthType: config.AuthTypeCert, ApiKey: "key-1", CertFile: "c"},
			map[string]string{"x-auth-scheme": "api-token", "x-auth-apikey": "key-1"}},
		{"token with client org", config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", IsPartner: true, PartnerOrg: "org-1"},
			map[string]string{"x-auth-scheme": "partner-api-token", "x-auth-apikey": "key-1", "x-auth-org": "org-1"}},
		{"oauth", config.Config{AuthType: config.AuthTypeOAuth, OAuthAccessToken: "access-1", OAuthRefreshToken: "refresh-1", OAuthExpiresAt: far},
			map[string]string{"x-auth-scheme": "api-oauth-token", "x-auth-access-token": "access-1"}},
		{"partner oauth with client org", config.Config{AuthType: config.AuthTypeOAuth, OAuthAccessToken: "access-1", OAuthRefreshToken: "refresh-1", OAuthExpiresAt: far, IsPartner: true, PartnerOrg: "org-1"},
			map[string]string{"x-auth-scheme": "partner-api-oauth-token", "x-auth-access-token": "access-1", "x-auth-org": "org-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := auth.Headers(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(h) != len(tc.want) {
				t.Errorf("headers = %v, want exactly %v", h, tc.want)
			}
			for k, v := range tc.want {
				if got := h.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestHeadersWithoutAPIKeyFails(t *testing.T) {
	setHome(t)
	_, err := auth.Headers(config.Config{AuthType: config.AuthTypeToken})
	if err == nil || !strings.Contains(err.Error(), "no API key configured") {
		t.Fatalf("err = %v, want the no-API-key error", err)
	}
}

func TestFreshTokenIsNotRefreshed(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-1")
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(10 * time.Minute)}, false)

	h, err := auth.Headers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get(auth.HeaderAccessToken); got != "access-1" {
		t.Fatalf("access token = %q, want access-1", got)
	}
	if n := ts.calls.Load(); n != 0 {
		t.Fatalf("token endpoint called %d times for a token with 10 minutes left", n)
	}
}

// A token with less than 60 s left is refreshed before it is used, and the
// rotated refresh token is persisted.
func TestRefreshBeforeExpiry(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-1")
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(30 * time.Second)}, false)

	h, err := auth.Headers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get(auth.HeaderAccessToken); got != "access-2" {
		t.Fatalf("access token = %q, want the refreshed access-2", got)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", n)
	}
	form := ts.forms[0]
	if form["grant_type"] != "refresh_token" || form["refresh_token"] != "refresh-1" || form["client_id"] != testClientID {
		t.Errorf("refresh form = %v", form)
	}
	if _, ok := form["client_secret"]; ok || ts.basic[0] {
		t.Errorf("public client sent a client secret (form %v, basic %v)", form, ts.basic[0])
	}
}

func TestRefreshPersistsRotatedToken(t *testing.T) {
	setHome(t)
	newTokenServer(t, "refresh-1")
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(30 * time.Second)}, false)

	if _, err := auth.Headers(cfg); err != nil {
		t.Fatal(err)
	}
	stored := config.LoadConfig(config.DefaultProfile)
	if stored.OAuthRefreshToken != "refresh-2" || stored.OAuthAccessToken != "access-2" {
		t.Fatalf("stored tokens = %q/%q, want the rotated access-2/refresh-2", stored.OAuthAccessToken, stored.OAuthRefreshToken)
	}
	if d := time.Until(time.Unix(stored.OAuthExpiresAt, 0)); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("stored expiry in %v, want ~1h", d)
	}
	if stored.AuthType != config.AuthTypeOAuth || stored.OAuthClientID != testClientID {
		t.Errorf("refresh changed other profile fields: %+v", stored)
	}
}

func TestRefreshSendsStoredClientSecret(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-1")
	writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(30 * time.Second)}, false)
	if err := config.SaveOAuthClient(config.DefaultProfile, testClientID, "test-client-secret", 0); err != nil {
		t.Fatal(err)
	}
	cfg := config.LoadConfig(config.DefaultProfile)

	if _, err := auth.Headers(cfg); err != nil {
		t.Fatal(err)
	}
	if got := ts.forms[0]["client_secret"]; got != "test-client-secret" {
		t.Fatalf("client_secret = %q, want the stored secret", got)
	}
}

// After taking the lock the refresh re-reads the credentials file: when another
// process already rotated the refresh token, its access token is used and the
// (now spent) refresh token in the caller's copy is never presented.
func TestRefreshAdoptsTokensRotatedByAnotherProcess(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-2")
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(30 * time.Second)}, false)

	// Another process refreshes after this one loaded its config.
	if err := config.SaveOAuthLogin(config.DefaultProfile, config.OAuthTokens{AccessToken: "access-2", RefreshToken: "refresh-2", ExpiresAt: expiresIn(time.Hour)}, false); err != nil {
		t.Fatal(err)
	}

	h, err := auth.Headers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get(auth.HeaderAccessToken); got != "access-2" {
		t.Fatalf("access token = %q, want access-2 from the other process", got)
	}
	if n := ts.calls.Load(); n != 0 {
		t.Fatalf("token endpoint called %d times; the rotated token on disk should have been used", n)
	}
}

func TestConcurrentRefreshMakesOneRequest(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-1")
	ts.delay = 100 * time.Millisecond
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(30 * time.Second)}, false)

	var wg sync.WaitGroup
	got := make([]string, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := auth.Headers(cfg)
			errs[i] = err
			if err == nil {
				got[i] = h.Get(auth.HeaderAccessToken)
			}
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != "access-2" {
			t.Errorf("goroutine %d: token %q, err %v", i, got[i], errs[i])
		}
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", n)
	}
}

func TestInvalidGrantClearsTokens(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "some-other-token")
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(30 * time.Second)}, true)

	_, err := auth.Headers(cfg)
	if !errors.Is(err, auth.ErrLoginRequired) || !strings.Contains(err.Error(), "rhombus login") {
		t.Fatalf("err = %v, want ErrLoginRequired telling the user to run rhombus login", err)
	}
	stored := config.LoadConfig(config.DefaultProfile)
	if stored.OAuthAccessToken != "" || stored.OAuthRefreshToken != "" || stored.OAuthExpiresAt != 0 {
		t.Fatalf("tokens not cleared: %q/%q/%d", stored.OAuthAccessToken, stored.OAuthRefreshToken, stored.OAuthExpiresAt)
	}
	if stored.AuthType != config.AuthTypeOAuth || !stored.IsPartner || stored.OAuthClientID != testClientID {
		t.Errorf("clearing tokens changed other profile fields: %+v", stored)
	}

	// The same process does not try again with the dead grant.
	if _, err := auth.Headers(cfg); !errors.Is(err, auth.ErrLoginRequired) {
		t.Fatalf("second call err = %v, want ErrLoginRequired", err)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", n)
	}
}

func TestRetry401(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-1")
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(time.Hour)}, false)
	sent, err := auth.Headers(cfg)
	if err != nil {
		t.Fatal(err)
	}

	retry, err := auth.Retry401(cfg, sent)
	if err != nil || !retry {
		t.Fatalf("Retry401 = %v, %v; want true, nil", retry, err)
	}
	h, _ := auth.Headers(cfg)
	if got := h.Get(auth.HeaderAccessToken); got != "access-2" {
		t.Fatalf("access token after 401 = %q, want access-2", got)
	}

	// A second 401 for the old token (e.g. a parallel request) reuses the
	// refreshed token instead of refreshing again.
	if retry, err := auth.Retry401(cfg, sent); err != nil || !retry {
		t.Fatalf("Retry401 = %v, %v; want true, nil", retry, err)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", n)
	}

	keyCfg := config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1"}
	if retry, err := auth.Retry401(keyCfg, http.Header{}); retry || err != nil {
		t.Fatalf("Retry401 for an API key = %v, %v; want false, nil", retry, err)
	}
}

// apiServer accepts only the given access token (or API key) and records the
// credentials and bodies it saw.
type apiServer struct {
	*httptest.Server
	mu     sync.Mutex
	tokens []string
	bodies []string
}

func newAPIServer(t *testing.T, accept string, tls bool) *apiServer {
	t.Helper()
	s := &apiServer{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred := r.Header.Get(auth.HeaderAccessToken) + r.Header.Get(auth.HeaderAPIKey)
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			r.Body.Read(body)
		}
		s.mu.Lock()
		s.tokens = append(s.tokens, cred)
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
		if cred != accept {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":true}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	})
	if tls {
		s.Server = httptest.NewTLSServer(h)
	} else {
		s.Server = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s
}

func TestAPICallRefreshesAndRetriesOnce401(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-1")
	api := newAPIServer(t, "access-2", false)
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(time.Hour)}, false)
	cfg.EndpointURL = api.URL

	res, err := client.APICall(cfg, "/api/org/getOrgV2", map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if res["ok"] != true {
		t.Fatalf("result = %v", res)
	}
	if strings.Join(api.tokens, ",") != "access-1,access-2" {
		t.Fatalf("API saw tokens %v, want access-1 then access-2", api.tokens)
	}
	if api.bodies[0] != `{"k":"v"}` || api.bodies[1] != api.bodies[0] {
		t.Fatalf("retried body = %q, want the original %q", api.bodies[1], api.bodies[0])
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", n)
	}
}

func TestAPICallGivesUpAfterOneRetry(t *testing.T) {
	setHome(t)
	newTokenServer(t, "refresh-1")
	api := newAPIServer(t, "never", false)
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(time.Hour)}, false)
	cfg.EndpointURL = api.URL

	_, err := client.APICall(cfg, "/api/org/getOrgV2", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
	if len(api.tokens) != 2 {
		t.Fatalf("API requests = %d, want 2", len(api.tokens))
	}
}

func TestAPICallDoesNotRetryAPIKey401(t *testing.T) {
	setHome(t)
	api := newAPIServer(t, "never", false)
	cfg := config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", EndpointURL: api.URL}

	if _, err := client.APICall(cfg, "/api/org/getOrgV2", map[string]any{}); err == nil {
		t.Fatal("want an error")
	}
	if len(api.tokens) != 1 {
		t.Fatalf("API requests = %d, want 1", len(api.tokens))
	}
}

func TestAPICallVerboseMasksAccessToken(t *testing.T) {
	setHome(t)
	api := newAPIServer(t, "access-secret-value-1234", false)
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-secret-value-1234", RefreshToken: "refresh-1", ExpiresAt: expiresIn(time.Hour)}, false)
	cfg.EndpointURL = api.URL
	cfg.Verbose = true

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	_, callErr := client.APICall(cfg, "/api/org/getOrgV2", map[string]any{})
	os.Stderr = orig
	w.Close()
	out := make([]byte, 64<<10)
	n, _ := r.Read(out)
	if callErr != nil {
		t.Fatal(callErr)
	}
	if strings.Contains(string(out[:n]), "access-secret-value") {
		t.Fatalf("verbose output contains the access token:\n%s", out[:n])
	}
	if !strings.Contains(string(out[:n]), "****1234") {
		t.Fatalf("verbose output lacks the masked token:\n%s", out[:n])
	}
}

func TestMediaClientRefreshesAndRetriesOnce401(t *testing.T) {
	setHome(t)
	ts := newTokenServer(t, "refresh-1")
	media := newAPIServer(t, "access-2", true)
	cfg := writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(time.Hour)}, false)

	c, err := client.GetMediaHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", media.URL+"/media/x.jpeg", nil)
	req.Header.Set("Cookie", "RHOMBUS_SESSIONID=RFT:fed-1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after the retry", resp.StatusCode)
	}
	if strings.Join(media.tokens, ",") != "access-1,access-2" {
		t.Fatalf("media saw tokens %v, want access-1 then access-2", media.tokens)
	}
	if n := ts.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", n)
	}
	if req.Header.Get(auth.HeaderAccessToken) != "" {
		t.Error("transport modified the caller's request")
	}
}

func TestMediaClientDoesNotRetryAPIKey401(t *testing.T) {
	setHome(t)
	media := newAPIServer(t, "never", true)
	c, err := client.GetMediaHTTPClient(config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(media.URL + "/media/x.jpeg")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || len(media.tokens) != 1 {
		t.Fatalf("status %d after %d requests, want one 401", resp.StatusCode, len(media.tokens))
	}
}

// Several CLI processes refreshing the same profile at once make one refresh
// request: the file lock serializes them and the re-read hands the rotated
// tokens to the others.
func TestCrossProcessRefreshMakesOneRequest(t *testing.T) {
	home := setHome(t)
	ts := newTokenServer(t, "refresh-1")
	ts.delay = 500 * time.Millisecond
	writeOAuthProfile(t, config.OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresIn(30 * time.Second)}, false)

	start := filepath.Join(t.TempDir(), "start")
	const n = 3
	cmds := make([]*exec.Cmd, n)
	outs := make([]*strings.Builder, n)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			helperEnv+"=1", "HOME="+home, "USERPROFILE="+home,
			"HELPER_TOKEN_URL="+ts.URL+"/oauth/token", "HELPER_START="+start)
		outs[i] = &strings.Builder{}
		cmd.Stdout, cmd.Stderr = outs[i], outs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	time.Sleep(200 * time.Millisecond) // let every helper load its (stale) config
	if err := os.WriteFile(start, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("helper %d: %v: %s", i, err, outs[i])
		} else if got := strings.TrimSpace(outs[i].String()); got != "access-2" {
			t.Errorf("helper %d used %q, want access-2", i, got)
		}
	}
	if c := ts.calls.Load(); c != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", c)
	}
	if rt := config.LoadConfig(config.DefaultProfile).OAuthRefreshToken; rt != "refresh-2" {
		t.Fatalf("stored refresh token = %q, want refresh-2", rt)
	}
}

const helperEnv = "RHOMBUS_AUTH_TEST_HELPER"

// TestMain runs the cross-process helper when re-executed by that test. Tests
// never reach a real auth host: refreshes go to a closed local port unless a
// test starts a tokenServer.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runRefreshHelper())
	}
	auth.SetTokenURL("http://127.0.0.1:1/oauth/token")
	os.Exit(m.Run())
}

func runRefreshHelper() int {
	auth.SetTokenURL(os.Getenv("HELPER_TOKEN_URL"))
	cfg := config.LoadConfig(config.DefaultProfile)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(os.Getenv("HELPER_START")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Println("start signal never came")
			return 1
		}
	}
	h, err := auth.Headers(cfg)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	fmt.Println(h.Get(auth.HeaderAccessToken))
	return 0
}
