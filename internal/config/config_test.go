package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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

func TestSaveOAuthLoginConvertsKeyProfile(t *testing.T) {
	setHome(t)
	if err := saveCredentialFields("work", map[string]string{
		"api_key": "key-1", "ws_api_key": "key-2", "auth_type": AuthTypeCert,
		"cert_file": "/c.crt", "key_file": "/c.key", "is_partner": "false",
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredentials(DefaultProfile, "other-key"); err != nil {
		t.Fatal(err)
	}

	tok := OAuthTokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: 1700000000}
	if err := SaveOAuthLogin("work", tok, true); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig("work")
	if cfg.AuthType != AuthTypeOAuth || !cfg.IsPartner {
		t.Errorf("auth type %q partner %v, want oauth partner", cfg.AuthType, cfg.IsPartner)
	}
	if cfg.OAuthAccessToken != "access-1" || cfg.OAuthRefreshToken != "refresh-1" || cfg.OAuthExpiresAt != 1700000000 {
		t.Errorf("tokens = %q/%q/%d", cfg.OAuthAccessToken, cfg.OAuthRefreshToken, cfg.OAuthExpiresAt)
	}
	if cfg.ApiKey != "" || cfg.WSApiKey != "" || cfg.CertFile != "" || cfg.KeyFile != "" {
		t.Errorf("key/cert settings survived the conversion: %+v", cfg)
	}
	if other := LoadConfig(DefaultProfile); other.ApiKey != "other-key" || other.AuthType != AuthTypeToken {
		t.Errorf("other profile changed: %+v", other)
	}
}

func TestUpdateOAuthTokens(t *testing.T) {
	setHome(t)
	if err := SaveOAuthLogin(DefaultProfile, OAuthTokens{AccessToken: "a1", RefreshToken: "r1", ExpiresAt: 10}, false); err != nil {
		t.Fatal(err)
	}

	var seen OAuthTokens
	if err := UpdateOAuthTokens(DefaultProfile, func(stored OAuthTokens) (OAuthTokens, bool, error) {
		seen = stored
		return OAuthTokens{AccessToken: "a2", RefreshToken: "r2", ExpiresAt: 20}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != (OAuthTokens{AccessToken: "a1", RefreshToken: "r1", ExpiresAt: 10}) {
		t.Errorf("fn saw %+v, want the stored tokens", seen)
	}
	if cfg := LoadConfig(DefaultProfile); cfg.OAuthRefreshToken != "r2" || cfg.OAuthAccessToken != "a2" || cfg.OAuthExpiresAt != 20 {
		t.Errorf("stored = %+v", cfg)
	}

	// save=false leaves the file alone; fn's error is returned.
	boom := errors.New("boom")
	if err := UpdateOAuthTokens(DefaultProfile, func(OAuthTokens) (OAuthTokens, bool, error) {
		return OAuthTokens{AccessToken: "x"}, false, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if cfg := LoadConfig(DefaultProfile); cfg.OAuthAccessToken != "a2" {
		t.Errorf("save=false wrote tokens: %+v", cfg)
	}

	// A zero value with save=true removes the tokens, even with an error.
	if err := UpdateOAuthTokens(DefaultProfile, func(OAuthTokens) (OAuthTokens, bool, error) {
		return OAuthTokens{}, true, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	cfg := LoadConfig(DefaultProfile)
	if cfg.OAuthAccessToken != "" || cfg.OAuthRefreshToken != "" || cfg.OAuthExpiresAt != 0 {
		t.Errorf("tokens not removed: %+v", cfg)
	}
	if cfg.AuthType != AuthTypeOAuth {
		t.Errorf("auth type = %q, want oauth kept", cfg.AuthType)
	}
}

func TestCredentialsWriteIsAtomicAndPrivate(t *testing.T) {
	setHome(t)
	if err := SaveCredentials(DefaultProfile, "key-1"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(CredentialsFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveOAuthLogin(DefaultProfile, OAuthTokens{AccessToken: "a", RefreshToken: "r"}, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(CredentialsFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("credentials file was rewritten in place; want temp file + rename")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != 0600 {
		t.Errorf("credentials mode = %v, want 0600", after.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(CredentialsFilePath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".credentials-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSaveOAuthClientReplacesSecret(t *testing.T) {
	setHome(t)
	if err := SaveOAuthClient(DefaultProfile, "client-1", "secret-1", 4242); err != nil {
		t.Fatal(err)
	}
	cfg := LoadConfig(DefaultProfile)
	if cfg.OAuthClientID != "client-1" || cfg.OAuthClientSecret != "secret-1" || cfg.CallbackPort != 4242 {
		t.Fatalf("client = %q/%q/%d", cfg.OAuthClientID, cfg.OAuthClientSecret, cfg.CallbackPort)
	}

	// A new public client must not inherit the old client's secret.
	if err := SaveOAuthClient(DefaultProfile, "client-2", "", 4343); err != nil {
		t.Fatal(err)
	}
	cfg = LoadConfig(DefaultProfile)
	if cfg.OAuthClientID != "client-2" || cfg.OAuthClientSecret != "" || cfg.CallbackPort != 4343 {
		t.Fatalf("client = %q/%q/%d, want client-2 with no secret", cfg.OAuthClientID, cfg.OAuthClientSecret, cfg.CallbackPort)
	}
}

func TestAPIKeyOverrideSwitchesOAuthProfileToToken(t *testing.T) {
	setHome(t)
	if err := SaveOAuthLogin(DefaultProfile, OAuthTokens{AccessToken: "a", RefreshToken: "r"}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RHOMBUS_API_KEY", "env-key")
	cfg := LoadConfig(DefaultProfile)
	if cfg.AuthType != AuthTypeToken || cfg.ApiKey != "env-key" {
		t.Fatalf("auth type %q key %q, want token env-key", cfg.AuthType, cfg.ApiKey)
	}

	// Key and cert profiles keep their auth type, as before.
	if err := saveCredentialFields("cert", map[string]string{"api_key": "k", "auth_type": AuthTypeCert}); err != nil {
		t.Fatal(err)
	}
	if cfg := LoadConfig("cert"); cfg.AuthType != AuthTypeCert {
		t.Fatalf("cert profile auth type = %q", cfg.AuthType)
	}
}

func TestRegion(t *testing.T) {
	setHome(t)
	if err := SaveConfig("eu", "", EUEndpointURL); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegion("eu", RegionEU); err != nil {
		t.Fatal(err)
	}
	cfg := LoadConfig("eu")
	if cfg.Region != RegionEU || cfg.RegionOrDefault() != RegionEU {
		t.Fatalf("region = %q / %q", cfg.Region, cfg.RegionOrDefault())
	}

	for _, tc := range []struct {
		endpoint, stored, want string
	}{
		{DefaultEndpointURL, "", RegionUS},
		{EUEndpointURL, "", RegionEU},
		{EUEndpointURL, RegionUS, RegionEU}, // a known endpoint wins
		{"http://localhost:8451", RegionEU, RegionEU},
		{"http://localhost:8451", "", ""},
	} {
		c := Config{EndpointURL: tc.endpoint, Region: tc.stored}
		if got := c.RegionOrDefault(); got != tc.want {
			t.Errorf("RegionOrDefault(%q, %q) = %q, want %q", tc.endpoint, tc.stored, got, tc.want)
		}
	}
	if WSHostForRegion(RegionEU) != "ws.eu.rhombussystems.com" || WSHostForRegion(RegionUS) != "ws.rhombussystems.com" {
		t.Error("unexpected WS hosts")
	}
}
