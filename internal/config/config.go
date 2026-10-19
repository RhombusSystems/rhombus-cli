package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/spf13/cobra"
	"gopkg.in/ini.v1"
)

const (
	DefaultEndpointURL = "https://api2.rhombussystems.com"
	EUEndpointURL      = "https://api2.eu.rhombussystems.com"
	DefaultOutput      = "json"
	DefaultProfile     = "default"

	AuthTypeToken = "token"
	AuthTypeCert  = "cert"
	AuthTypeOAuth = "oauth"

	RegionUS = "us"
	RegionEU = "eu"
)

// EndpointForRegion returns the API endpoint URL for the given region.
// Unknown regions fall back to the US endpoint.
func EndpointForRegion(region string) string {
	switch region {
	case RegionEU:
		return EUEndpointURL
	default:
		return DefaultEndpointURL
	}
}

// AuthBaseURLForRegion returns the OAuth/auth service base URL for the given region.
func AuthBaseURLForRegion(region string) string {
	switch region {
	case RegionEU:
		return "https://auth.eu.rhombussystems.com"
	default:
		return "https://auth.rhombussystems.com"
	}
}

// AuthWebBaseURLForRegion returns the base URL of the OAuth service that handles
// dynamic client registration (/oauth/register) and the token endpoint
// (/oauth/token) for the given region.
func AuthWebBaseURLForRegion(region string) string {
	switch region {
	case RegionEU:
		return "https://auth-web.eu.rhombussystems.com"
	default:
		return "https://auth-web.rhombussystems.com"
	}
}

// WSHostForRegion returns the event WebSocket host for the given region.
func WSHostForRegion(region string) string {
	switch region {
	case RegionEU:
		return "ws.eu.rhombussystems.com"
	default:
		return "ws.rhombussystems.com"
	}
}

// ConsoleBaseURLForRegion returns the web console base URL for the given region.
func ConsoleBaseURLForRegion(region string) string {
	switch region {
	case RegionEU:
		return "https://console.eu.rhombussystems.com"
	default:
		return "https://console.rhombussystems.com"
	}
}

// RegionForEndpoint returns the region matching the given endpoint URL,
// or an empty string if the endpoint is custom.
func RegionForEndpoint(endpoint string) string {
	switch endpoint {
	case DefaultEndpointURL:
		return RegionUS
	case EUEndpointURL:
		return RegionEU
	default:
		return ""
	}
}

type Config struct {
	ApiKey      string
	WSApiKey    string // token-based API key for websocket (cert keys don't work with WS service)
	EndpointURL string
	Region      string // "us" or "eu" as chosen at login; see RegionOrDefault
	Output      string
	Profile     string
	AuthType    string // "token", "cert" or "oauth"
	CertFile    string // path to client certificate PEM
	KeyFile     string // path to client private key PEM
	IsPartner   bool   // whether this is a partner-level credential
	PartnerOrg  string // client org UUID for partner emulation (set via --partner-org flag)
	Verbose     bool   // print full HTTP request and response details

	// OAuth browser-login client. These are obtained automatically via dynamic
	// client registration (DCR) on first `rhombus login` and persisted so the
	// CLI reuses the same registered client on subsequent logins. They are not
	// user-configured. CallbackPort is the loopback port the registered client's
	// redirect URI is bound to (exact-match), so changing it forces re-registration.
	OAuthClientID     string
	OAuthClientSecret string // empty for a public (PKCE-only) client
	CallbackPort      int

	// OAuth tokens from `rhombus login` (AuthType "oauth"). ExpiresAt is the access
	// token's expiry in Unix seconds (0 = unknown).
	OAuthAccessToken  string
	OAuthRefreshToken string
	OAuthExpiresAt    int64

	credProfile string // credentials section the values above were read from
}

// CredentialsProfile returns the credentials-file section this config was loaded
// from, which is where refreshed OAuth tokens are written back.
func (c Config) CredentialsProfile() string {
	if c.credProfile != "" {
		return c.credProfile
	}
	return c.Profile
}

// RegionOrDefault returns the region of the configured endpoint, falling back to
// the region stored at login for custom endpoints ("" when neither is known).
func (c Config) RegionOrDefault() string {
	if r := RegionForEndpoint(c.EndpointURL); r != "" {
		return r
	}
	return c.Region
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".rhombus")
}

func ConfigFilePath() string {
	return filepath.Join(configDir(), "config")
}

func CredentialsFilePath() string {
	return filepath.Join(configDir(), "credentials")
}

func LoadConfig(profile string) Config {
	cfg := Config{
		EndpointURL: DefaultEndpointURL,
		Output:      DefaultOutput,
		Profile:     profile,
		AuthType:    AuthTypeToken,
		credProfile: profile,
	}

	// Load config file
	if f, err := ini.Load(ConfigFilePath()); err == nil {
		section := sectionName(f, profile)
		if s, err := f.GetSection(section); err == nil {
			if k, err := s.GetKey("output"); err == nil {
				cfg.Output = k.String()
			}
			if k, err := s.GetKey("endpoint_url"); err == nil {
				cfg.EndpointURL = k.String()
			}
			if k, err := s.GetKey("region"); err == nil {
				cfg.Region = k.String()
			}
			if k, err := s.GetKey("oauth_client_id"); err == nil {
				cfg.OAuthClientID = k.String()
			}
			if k, err := s.GetKey("callback_port"); err == nil {
				if p, err := k.Int(); err == nil {
					cfg.CallbackPort = p
				}
			}
		}
	}

	// Load credentials file
	if f, err := ini.Load(CredentialsFilePath()); err == nil {
		section := profile
		if s, err := f.GetSection(section); err == nil {
			if k, err := s.GetKey("api_key"); err == nil {
				cfg.ApiKey = k.String()
			}
			if k, err := s.GetKey("ws_api_key"); err == nil {
				cfg.WSApiKey = k.String()
			}
			if k, err := s.GetKey("auth_type"); err == nil {
				cfg.AuthType = k.String()
			}
			if k, err := s.GetKey("cert_file"); err == nil {
				cfg.CertFile = k.String()
			}
			if k, err := s.GetKey("key_file"); err == nil {
				cfg.KeyFile = k.String()
			}
			if k, err := s.GetKey("is_partner"); err == nil {
				cfg.IsPartner = k.String() == "true"
			}
			if k, err := s.GetKey("oauth_client_secret"); err == nil {
				cfg.OAuthClientSecret = k.String()
			}
			t := readOAuthTokens(s)
			cfg.OAuthAccessToken, cfg.OAuthRefreshToken, cfg.OAuthExpiresAt = t.AccessToken, t.RefreshToken, t.ExpiresAt
		}
	}

	// Env var overrides
	if v := os.Getenv("RHOMBUS_API_KEY"); v != "" {
		cfg.overrideApiKey(v)
	}
	if v := os.Getenv("RHOMBUS_PROFILE"); v != "" {
		cfg.Profile = v
	}
	if v := os.Getenv("RHOMBUS_OUTPUT"); v != "" {
		cfg.Output = v
	}
	if v := os.Getenv("RHOMBUS_ENDPOINT_URL"); v != "" {
		cfg.EndpointURL = v
	}

	return cfg
}

// overrideApiKey applies an explicit API key (--api-key / RHOMBUS_API_KEY). An
// OAuth profile then authenticates with that key instead of its tokens.
func (c *Config) overrideApiKey(key string) {
	c.ApiKey = key
	if c.AuthType == AuthTypeOAuth {
		c.AuthType = AuthTypeToken
	}
}

func LoadFromCmd(cmd *cobra.Command) Config {
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")
	if profile == "" {
		profile = DefaultProfile
	}

	cfg := LoadConfig(profile)

	// CLI flag overrides
	if v, _ := cmd.Root().PersistentFlags().GetString("api-key"); v != "" {
		cfg.overrideApiKey(v)
	}
	if v, _ := cmd.Root().PersistentFlags().GetString("endpoint-url"); v != "" {
		cfg.EndpointURL = v
	}
	if v, _ := cmd.Root().PersistentFlags().GetString("output"); v != "" {
		cfg.Output = v
	}
	if v, _ := cmd.Root().PersistentFlags().GetString("partner-org"); v != "" {
		cfg.PartnerOrg = v
	}
	if v, _ := cmd.Root().PersistentFlags().GetBool("verbose"); v {
		cfg.Verbose = true
	}

	return cfg
}

func SaveConfig(profile, output, endpointURL string) error {
	fields := map[string]string{}
	if output != "" {
		fields["output"] = output
	}
	if endpointURL != "" {
		fields["endpoint_url"] = endpointURL
	}
	return saveConfigFields(profile, fields)
}

// SaveOAuthClient persists the dynamically-registered OAuth client for a profile.
// The (non-secret) client_id and callback_port live in the config file; the
// client_secret lives in the 0600 credentials file. A zero port is skipped. The
// secret always replaces the previous client's: an empty secret (a public PKCE
// client) removes any stored one.
func SaveOAuthClient(profile, clientID, clientSecret string, callbackPort int) error {
	cfgFields := map[string]string{"oauth_client_id": clientID}
	if callbackPort != 0 {
		cfgFields["callback_port"] = strconv.Itoa(callbackPort)
	}
	if err := saveConfigFields(profile, cfgFields); err != nil {
		return err
	}
	return updateCredentials(profile, func(s *ini.Section) {
		if clientSecret != "" {
			s.Key("oauth_client_secret").SetValue(clientSecret)
		} else {
			s.DeleteKey("oauth_client_secret")
		}
	})
}

// SaveRegion stores the region chosen at login in the profile's config section.
func SaveRegion(profile, region string) error {
	return saveConfigFields(profile, map[string]string{"region": region})
}

// saveConfigFields merges the given keys into a profile's section of the config
// file without clobbering keys it doesn't set.
func saveConfigFields(profile string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}

	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	path := ConfigFilePath()
	f, err := ini.Load(path)
	if err != nil {
		f = ini.Empty()
	}

	section := profile
	if profile != DefaultProfile {
		section = "profile " + profile
	}

	s, err := f.NewSection(section)
	if err != nil {
		return err
	}
	for k, v := range fields {
		s.Key(k).SetValue(v)
	}

	return f.SaveTo(path)
}

func SaveCredentials(profile, apiKey string) error {
	return saveCredentialFields(profile, map[string]string{
		"api_key":   apiKey,
		"auth_type": AuthTypeToken,
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// OAuthTokens is the OAuth token set stored for a profile.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // access token expiry, Unix seconds (0 = unknown)
}

func readOAuthTokens(s *ini.Section) OAuthTokens {
	t := OAuthTokens{
		AccessToken:  s.Key("oauth_access_token").String(),
		RefreshToken: s.Key("oauth_refresh_token").String(),
	}
	t.ExpiresAt, _ = s.Key("oauth_expires_at").Int64()
	return t
}

// writeOAuthTokens stores t in s; a zero t removes the stored tokens.
func writeOAuthTokens(s *ini.Section, t OAuthTokens) {
	if t == (OAuthTokens{}) {
		s.DeleteKey("oauth_access_token")
		s.DeleteKey("oauth_refresh_token")
		s.DeleteKey("oauth_expires_at")
		return
	}
	s.Key("oauth_access_token").SetValue(t.AccessToken)
	s.Key("oauth_refresh_token").SetValue(t.RefreshToken)
	s.Key("oauth_expires_at").SetValue(strconv.FormatInt(t.ExpiresAt, 10))
}

// SaveOAuthLogin stores a browser login's tokens and switches the profile to
// OAuth. API-key and certificate settings from an earlier login are removed
// from the profile (the keys themselves are not revoked).
func SaveOAuthLogin(profile string, t OAuthTokens, isPartner bool) error {
	return updateCredentials(profile, func(s *ini.Section) {
		for _, k := range []string{"api_key", "ws_api_key", "cert_file", "key_file"} {
			s.DeleteKey(k)
		}
		s.Key("auth_type").SetValue(AuthTypeOAuth)
		s.Key("is_partner").SetValue(boolStr(isPartner))
		writeOAuthTokens(s, t)
	})
}

// UpdateOAuthTokens runs fn while holding an exclusive lock on the credentials
// file, passing the profile's tokens as stored on disk at that moment. When fn
// returns save=true its tokens are written (a zero value removes them) before the
// lock is released, even if fn also returns an error. This is how a refresh
// stays safe across processes: whoever holds the lock sees the latest rotated
// refresh token.
func UpdateOAuthTokens(profile string, fn func(stored OAuthTokens) (next OAuthTokens, save bool, err error)) error {
	unlock, err := lockCredentials()
	if err != nil {
		return err
	}
	defer unlock()

	f, err := loadCredentialsFile()
	if err != nil {
		return err
	}
	s := f.Section(profile)
	next, save, fnErr := fn(readOAuthTokens(s))
	if save {
		writeOAuthTokens(s, next)
		if err := writeCredentialsFile(f); err != nil {
			return err
		}
	}
	return fnErr
}

func saveCredentialFields(profile string, fields map[string]string) error {
	return updateCredentials(profile, func(s *ini.Section) {
		for k, v := range fields {
			s.Key(k).SetValue(v)
		}
	})
}

// updateCredentials applies edit to a profile's credentials section under the
// credentials lock and writes the file atomically.
func updateCredentials(profile string, edit func(*ini.Section)) error {
	unlock, err := lockCredentials()
	if err != nil {
		return err
	}
	defer unlock()

	f, err := loadCredentialsFile()
	if err != nil {
		return err
	}
	edit(f.Section(profile))
	return writeCredentialsFile(f)
}

// loadCredentialsFile reads the credentials file for an update. A missing file
// starts empty; an unreadable one is an error rather than being overwritten.
func loadCredentialsFile() (*ini.File, error) {
	path := CredentialsFilePath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ini.Empty(), nil
	}
	f, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return f, nil
}

// writeCredentialsFile replaces the credentials file via a temp file + rename,
// so a reader never sees a partially written file. Callers hold the lock.
func writeCredentialsFile(f *ini.File) error {
	path := CredentialsFilePath()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := f.WriteTo(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// credentialsMu serializes credential updates within this process; the file
// lock in lockCredentials serializes them across processes.
var credentialsMu sync.Mutex

// lockCredentials takes an exclusive lock on credentials.lock next to the
// credentials file. A separate lock file is used because the credentials file
// itself is replaced on every write.
func lockCredentials() (unlock func(), err error) {
	credentialsMu.Lock()
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		credentialsMu.Unlock()
		return nil, err
	}
	lf, err := os.OpenFile(CredentialsFilePath()+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		credentialsMu.Unlock()
		return nil, err
	}
	if err := lockFile(lf); err != nil {
		lf.Close()
		credentialsMu.Unlock()
		return nil, fmt.Errorf("locking credentials: %w", err)
	}
	return func() {
		unlockFile(lf)
		lf.Close()
		credentialsMu.Unlock()
	}, nil
}

func sectionName(f *ini.File, profile string) string {
	if profile == DefaultProfile {
		return DefaultProfile
	}
	name := "profile " + profile
	if _, err := f.GetSection(name); err == nil {
		return name
	}
	return profile
}

func PrintConfig(cfg Config) {
	fmt.Printf("Profile:      %s\n", cfg.Profile)
	fmt.Printf("Endpoint URL: %s\n", cfg.EndpointURL)
	fmt.Printf("Output:       %s\n", cfg.Output)
	fmt.Printf("Auth Type:    %s\n", cfg.AuthType)
	if cfg.ApiKey != "" {
		fmt.Printf("API Key:      ****%s\n", cfg.ApiKey[max(0, len(cfg.ApiKey)-4):])
	} else {
		fmt.Println("API Key:      (not set)")
	}
	if cfg.AuthType == AuthTypeCert {
		fmt.Printf("Cert File:    %s\n", cfg.CertFile)
		fmt.Printf("Key File:     %s\n", cfg.KeyFile)
	}
}
