// Package auth builds the credential headers for every request the CLI sends to
// Rhombus (REST API, media hosts and the event WebSocket) and keeps OAuth access
// tokens fresh.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

const (
	HeaderScheme      = "x-auth-scheme"
	HeaderAPIKey      = "x-auth-apikey"
	HeaderAccessToken = "x-auth-access-token"
	HeaderOrg         = "x-auth-org"

	// RefreshLeeway is how long before expiry an access token is refreshed.
	RefreshLeeway = 60 * time.Second
)

// ErrLoginRequired means the profile has no usable OAuth credentials.
var ErrLoginRequired = errors.New("login required")

// now and tokenURL are replaced in tests.
var (
	now      = time.Now
	tokenURL = func(cfg config.Config) string {
		return config.AuthWebBaseURLForRegion(cfg.RegionOrDefault()) + "/oauth/token"
	}
	refreshClient = &http.Client{Timeout: 30 * time.Second}
)

// UsesClientCert reports whether cfg authenticates with an mTLS client certificate.
func UsesClientCert(cfg config.Config) bool {
	return cfg.AuthType == config.AuthTypeCert && cfg.CertFile != "" && cfg.KeyFile != ""
}

// Scheme returns the x-auth-scheme value for cfg's credential kind.
func Scheme(cfg config.Config) string {
	var scheme string
	switch {
	case cfg.AuthType == config.AuthTypeOAuth:
		scheme = "api-oauth-token"
	case UsesClientCert(cfg):
		scheme = "api"
	default:
		scheme = "api-token"
	}
	if cfg.IsPartner {
		return "partner-" + scheme
	}
	return scheme
}

// Headers returns the credential headers for cfg: the scheme, the API key or
// OAuth access token, and x-auth-org when a client org is selected. An OAuth
// access token with less than RefreshLeeway left is refreshed first.
func Headers(cfg config.Config) (http.Header, error) {
	h := http.Header{}
	h.Set(HeaderScheme, Scheme(cfg))
	if cfg.AuthType == config.AuthTypeOAuth {
		tok, err := freshToken(cfg)
		if err != nil {
			return nil, err
		}
		h.Set(HeaderAccessToken, tok.AccessToken)
	} else {
		if cfg.ApiKey == "" {
			return nil, fmt.Errorf("no API key configured. Run 'rhombus login' or 'rhombus configure'")
		}
		h.Set(HeaderAPIKey, cfg.ApiKey)
	}
	if cfg.PartnerOrg != "" {
		h.Set(HeaderOrg, cfg.PartnerOrg)
	}
	return h, nil
}

// Retry401 handles an HTTP 401 for a request that was sent with the headers
// `sent`. For OAuth profiles it refreshes the rejected access token and returns
// true: the caller retries the request once with new Headers. Key and
// certificate profiles never retry. A non-nil error says why the token could not
// be refreshed (ErrLoginRequired: the user must run `rhombus login`).
func Retry401(cfg config.Config, sent http.Header) (bool, error) {
	if cfg.AuthType != config.AuthTypeOAuth {
		return false, nil
	}
	if _, err := refresh(cfg, sent.Get(HeaderAccessToken)); err != nil {
		return false, err
	}
	return true, nil
}

// Transport is an http.RoundTripper that adds Headers to each request and, for
// OAuth profiles, refreshes the token and retries once on HTTP 401.
type Transport struct {
	Cfg  config.Config
	Base http.RoundTripper // nil = http.DefaultTransport
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	h, err := Headers(t.Cfg)
	if err != nil {
		return nil, err
	}
	resp, err := base.RoundTrip(withHeaders(req, h))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return resp, nil // the body cannot be replayed
	}
	retry, err := Retry401(t.Cfg, h)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	if !retry {
		return resp, nil
	}
	if h, err = Headers(t.Cfg); err != nil {
		resp.Body.Close()
		return nil, err
	}
	again := withHeaders(req, h)
	if req.GetBody != nil {
		if again.Body, err = req.GetBody(); err != nil {
			resp.Body.Close()
			return nil, err
		}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return base.RoundTrip(again)
}

func withHeaders(req *http.Request, h http.Header) *http.Request {
	r := req.Clone(req.Context())
	for k, v := range h {
		r.Header[k] = v
	}
	return r
}

// Tokens refreshed or adopted by this process, newer than the copy in a Config
// loaded at startup. Keyed by credentials file + profile.
var (
	cacheMu   sync.Mutex
	cache     = map[string]config.OAuthTokens{}
	loggedOut = map[string]bool{}
)

func cacheKey(cfg config.Config) string {
	return config.CredentialsFilePath() + "\x00" + cfg.CredentialsProfile()
}

// current returns the newest tokens this process knows for cfg's profile.
func current(cfg config.Config) (config.OAuthTokens, bool) {
	tok := config.OAuthTokens{
		AccessToken:  cfg.OAuthAccessToken,
		RefreshToken: cfg.OAuthRefreshToken,
		ExpiresAt:    cfg.OAuthExpiresAt,
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	key := cacheKey(cfg)
	if c, ok := cache[key]; ok && c.ExpiresAt >= tok.ExpiresAt {
		tok = c
	}
	return tok, loggedOut[key]
}

func remember(cfg config.Config, tok config.OAuthTokens) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	key := cacheKey(cfg)
	if tok == (config.OAuthTokens{}) {
		delete(cache, key)
		loggedOut[key] = true
		return
	}
	cache[key] = tok
	delete(loggedOut, key)
}

func expiringSoon(tok config.OAuthTokens) bool {
	return tok.ExpiresAt != 0 && now().Add(RefreshLeeway).Unix() >= tok.ExpiresAt
}

func freshToken(cfg config.Config) (config.OAuthTokens, error) {
	tok, out := current(cfg)
	if out {
		return tok, loginRequired(cfg)
	}
	if tok.AccessToken != "" && !expiringSoon(tok) {
		return tok, nil
	}
	return refresh(cfg, "")
}

// refresh obtains a new access token. rejected is the access token a server just
// answered 401 to ("" when refreshing because the token is about to expire).
//
// It runs under the credentials file lock and decides from the tokens on disk,
// not the caller's copy: when another process (or goroutine) already rotated the
// refresh token, its still-valid access token is used and no request is made.
func refresh(cfg config.Config, rejected string) (config.OAuthTokens, error) {
	var result config.OAuthTokens
	err := config.UpdateOAuthTokens(cfg.CredentialsProfile(), func(stored config.OAuthTokens) (config.OAuthTokens, bool, error) {
		if stored.RefreshToken == "" {
			result = stored
			return stored, false, loginRequired(cfg)
		}
		if stored.AccessToken != "" && stored.AccessToken != rejected && !expiringSoon(stored) {
			result = stored // already refreshed elsewhere
			return stored, false, nil
		}
		next, err := requestRefresh(cfg, stored.RefreshToken)
		if errors.Is(err, errInvalidGrant) {
			result = config.OAuthTokens{}
			return result, true, loginRequired(cfg)
		}
		if err != nil {
			return stored, false, err
		}
		result = next
		return next, true, nil
	})
	if err == nil || errors.Is(err, ErrLoginRequired) {
		remember(cfg, result)
	}
	if err != nil {
		return config.OAuthTokens{}, err
	}
	return result, nil
}

func loginRequired(cfg config.Config) error {
	cmd := "rhombus login"
	if p := cfg.CredentialsProfile(); p != "" && p != config.DefaultProfile {
		cmd += " --profile " + p
	}
	return fmt.Errorf("%w: the Rhombus login for profile %q has expired or was revoked. Run '%s' to sign in again",
		ErrLoginRequired, cfg.CredentialsProfile(), cmd)
}

var errInvalidGrant = errors.New("invalid_grant")

// requestRefresh performs the OAuth refresh_token grant (RFC 6749 §6). Public
// clients send only client_id; a client with a stored secret also sends it.
func requestRefresh(cfg config.Config, refreshToken string) (config.OAuthTokens, error) {
	if cfg.OAuthClientID == "" {
		return config.OAuthTokens{}, fmt.Errorf("%w: no OAuth client is registered for profile %q. Run 'rhombus login --force'",
			ErrLoginRequired, cfg.CredentialsProfile())
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {cfg.OAuthClientID},
	}
	if cfg.OAuthClientSecret != "" {
		form.Set("client_secret", cfg.OAuthClientSecret)
	}
	req, err := http.NewRequest("POST", tokenURL(cfg), strings.NewReader(form.Encode()))
	if err != nil {
		return config.OAuthTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := refreshClient.Do(req)
	if err != nil {
		return config.OAuthTokens{}, fmt.Errorf("refreshing OAuth token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return config.OAuthTokens{}, fmt.Errorf("refreshing OAuth token: %w", err)
	}

	var tr struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode != http.StatusOK {
		if tr.Error == "invalid_grant" {
			return config.OAuthTokens{}, errInvalidGrant
		}
		if tr.Error != "" {
			return config.OAuthTokens{}, fmt.Errorf("refreshing OAuth token: HTTP %d: %s: %s", resp.StatusCode, tr.Error, tr.ErrorDescription)
		}
		return config.OAuthTokens{}, fmt.Errorf("refreshing OAuth token: HTTP %d", resp.StatusCode)
	}
	if tr.AccessToken == "" {
		return config.OAuthTokens{}, fmt.Errorf("refreshing OAuth token: no access token in response")
	}
	next := config.OAuthTokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    ExpiresAt(tr.ExpiresIn),
	}
	if next.RefreshToken == "" {
		next.RefreshToken = refreshToken // server did not rotate it
	}
	return next, nil
}

// ExpiresAt converts a token response's expires_in (seconds) to a Unix expiry;
// 0 when the server did not say.
func ExpiresAt(expiresIn int64) int64 {
	if expiresIn <= 0 {
		return 0
	}
	return now().Unix() + expiresIn
}
