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
)

type Config struct {
	ApiKey      string
	EndpointURL string
	Output      string
	Profile     string
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
	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	path := CredentialsFilePath()
	f, err := ini.Load(path)
	if err != nil {
		f = ini.Empty()
	}

	s, err := f.NewSection(profile)
	if err != nil {
		return err
	}
	s.Key("api_key").SetValue(apiKey)

	if err := f.SaveTo(path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func sectionName(f *ini.File, profile string) string {
	// Try "profile <name>" first (AWS-style), then bare name
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
	if cfg.ApiKey != "" {
		fmt.Printf("API Key:      ****%s\n", cfg.ApiKey[max(0, len(cfg.ApiKey)-4):])
	} else {
		fmt.Println("API Key:      (not set)")
	}
}
