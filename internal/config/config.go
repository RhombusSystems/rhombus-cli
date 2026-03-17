package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/ini.v1"
)

const (
	DefaultEndpointURL = "https://api2.rhombussystems.com"
	DefaultOutput      = "json"
	DefaultProfile     = "default"

	AuthTypeToken = "token"
	AuthTypeCert  = "cert"
)

type Config struct {
	ApiKey      string
	EndpointURL string
	Output      string
	Profile     string
	AuthType    string // "token" or "cert"
	CertFile    string // path to client certificate PEM
	KeyFile     string // path to client private key PEM
	IsPartner   bool   // whether this is a partner-level credential
	PartnerOrg  string // client org UUID for partner emulation (set via --partner-org flag)
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

// ProfileCertDir returns the directory for storing cert/key files for a profile.
func ProfileCertDir(profile string) string {
	return filepath.Join(configDir(), "certs", profile)
}

func LoadConfig(profile string) Config {
	cfg := Config{
		EndpointURL: DefaultEndpointURL,
		Output:      DefaultOutput,
		Profile:     profile,
		AuthType:    AuthTypeToken,
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
		}
	}

	// Load credentials file
	if f, err := ini.Load(CredentialsFilePath()); err == nil {
		section := profile
		if s, err := f.GetSection(section); err == nil {
			if k, err := s.GetKey("api_key"); err == nil {
				cfg.ApiKey = k.String()
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
		}
	}

	// Env var overrides
	if v := os.Getenv("RHOMBUS_API_KEY"); v != "" {
		cfg.ApiKey = v
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

func LoadFromCmd(cmd *cobra.Command) Config {
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")
	if profile == "" {
		profile = DefaultProfile
	}

	cfg := LoadConfig(profile)

	// CLI flag overrides
	if v, _ := cmd.Root().PersistentFlags().GetString("api-key"); v != "" {
		cfg.ApiKey = v
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

	return cfg
}

func SaveConfig(profile, output, endpointURL string) error {
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
	if output != "" {
		s.Key("output").SetValue(output)
	}
	if endpointURL != "" {
		s.Key("endpoint_url").SetValue(endpointURL)
	}

	return f.SaveTo(path)
}

func SaveCredentials(profile, apiKey string) error {
	return saveCredentialFields(profile, map[string]string{
		"api_key":   apiKey,
		"auth_type": AuthTypeToken,
	})
}

func SaveTokenCredentials(profile, apiKey string, isPartner bool) error {
	return saveCredentialFields(profile, map[string]string{
		"api_key":    apiKey,
		"auth_type":  AuthTypeToken,
		"is_partner": boolStr(isPartner),
	})
}

func SaveCertCredentials(profile, apiKey, certFile, keyFile string, isPartner bool) error {
	return saveCredentialFields(profile, map[string]string{
		"api_key":    apiKey,
		"auth_type":  AuthTypeCert,
		"cert_file":  certFile,
		"key_file":   keyFile,
		"is_partner": boolStr(isPartner),
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func SaveRefreshToken(profile, refreshToken string) error {
	return saveCredentialFields(profile, map[string]string{
		"refresh_token": refreshToken,
	})
}

func saveCredentialFields(profile string, fields map[string]string) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	path := CredentialsFilePath()
	f, err := ini.Load(path)
	if err != nil {
		f = ini.Empty()
	}

	s, err := f.GetSection(profile)
	if err != nil {
		s, err = f.NewSection(profile)
		if err != nil {
			return err
		}
	}

	for k, v := range fields {
		s.Key(k).SetValue(v)
	}

	if err := f.SaveTo(path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
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
