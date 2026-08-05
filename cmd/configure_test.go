package cmd

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

const testAPIKey = "test-key-do-not-use-01"

func swapStdin(t *testing.T, input string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		f.Close()
	})
}

func TestPromptReturnsEmptyWhenDefaultAccepted(t *testing.T) {
	got := prompt(bufio.NewReader(strings.NewReader("\n")), "Rhombus API Key", maskKey(testAPIKey))
	if got != "" {
		t.Fatalf("prompt = %q, want %q: accepting the default must not echo the displayed value back to the caller", got, "")
	}
}

func TestPromptReturnsTrimmedInput(t *testing.T) {
	got := prompt(bufio.NewReader(strings.NewReader("  json  \n")), "Default output format", "table")
	if got != "json" {
		t.Fatalf("prompt = %q, want %q", got, "json")
	}
}

func TestOrDefault(t *testing.T) {
	for _, tc := range []struct {
		input, fallback, want string
	}{
		{"", "table", "table"},
		{"json", "table", "json"},
		{"", "", ""},
	} {
		if got := orDefault(tc.input, tc.fallback); got != tc.want {
			t.Errorf("orDefault(%q, %q) = %q, want %q", tc.input, tc.fallback, got, tc.want)
		}
	}
}

func TestConfigureKeepsApiKeyWhenPromptSkipped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveCredentials(config.DefaultProfile, testAPIKey); err != nil {
		t.Fatal(err)
	}

	swapStdin(t, "\n\n\n\n")
	if err := configureCmd.RunE(configureCmd, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	got := config.LoadConfig(config.DefaultProfile).ApiKey
	if got == maskKey(testAPIKey) {
		t.Fatalf("api key = %q: configure saved its own mask over the stored key", got)
	}
	if got != testAPIKey {
		t.Fatalf("api key = %q, want %q", got, testAPIKey)
	}
}

func TestConfigureSavesTypedApiKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveCredentials(config.DefaultProfile, testAPIKey); err != nil {
		t.Fatal(err)
	}

	const replacement = "replacement-key-000001"
	swapStdin(t, replacement+"\n\n\n\n")
	if err := configureCmd.RunE(configureCmd, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	if got := config.LoadConfig(config.DefaultProfile).ApiKey; got != replacement {
		t.Fatalf("api key = %q, want %q", got, replacement)
	}
}

func TestConfigureKeepsOutputAndEndpointWhenPromptSkipped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveConfig(config.DefaultProfile, "table", config.EUEndpointURL); err != nil {
		t.Fatal(err)
	}

	swapStdin(t, "\n\n\n\n")
	if err := configureCmd.RunE(configureCmd, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	cfg := config.LoadConfig(config.DefaultProfile)
	if cfg.Output != "table" {
		t.Errorf("output = %q, want %q", cfg.Output, "table")
	}
	if cfg.EndpointURL != config.EUEndpointURL {
		t.Errorf("endpoint = %q, want %q", cfg.EndpointURL, config.EUEndpointURL)
	}
}

const itgEndpoint = "https://api2.itg.rhombussystems.com"

// Every endpoint class must survive an Enter-through, including the custom ones a
// region cannot describe — silently repointing those at prod sends commands to the
// wrong environment while appearing to succeed.
func TestConfigureKeepsEveryEndpointClassWhenPromptSkipped(t *testing.T) {
	for _, endpoint := range []string{
		config.DefaultEndpointURL,
		config.EUEndpointURL,
		itgEndpoint,
		"http://localhost:8451",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := config.SaveConfig(config.DefaultProfile, "json", endpoint); err != nil {
				t.Fatal(err)
			}

			swapStdin(t, "\n\n\n\n")
			if err := configureCmd.RunE(configureCmd, nil); err != nil {
				t.Fatalf("configure: %v", err)
			}

			if got := config.LoadConfig(config.DefaultProfile).EndpointURL; got != endpoint {
				t.Fatalf("endpoint = %q, want %q", got, endpoint)
			}
		})
	}
}

func TestConfigureSwitchesEndpointWhenRegionTyped(t *testing.T) {
	for _, tc := range []struct {
		name, start, typed, want string
	}{
		{"custom to eu", itgEndpoint, "eu", config.EUEndpointURL},
		{"custom to us", itgEndpoint, "us", config.DefaultEndpointURL},
		{"eu to us", config.EUEndpointURL, "us", config.DefaultEndpointURL},
		{"us to eu", config.DefaultEndpointURL, "eu", config.EUEndpointURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := config.SaveConfig(config.DefaultProfile, "json", tc.start); err != nil {
				t.Fatal(err)
			}

			swapStdin(t, "\n\n"+tc.typed+"\n\n")
			if err := configureCmd.RunE(configureCmd, nil); err != nil {
				t.Fatalf("configure: %v", err)
			}

			if got := config.LoadConfig(config.DefaultProfile).EndpointURL; got != tc.want {
				t.Fatalf("endpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigureSavesTypedEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveConfig(config.DefaultProfile, "json", itgEndpoint); err != nil {
		t.Fatal(err)
	}

	const typed = "https://api2.staging.rhombussystems.com"
	swapStdin(t, "\n\n\n"+typed+"\n")
	if err := configureCmd.RunE(configureCmd, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	if got := config.LoadConfig(config.DefaultProfile).EndpointURL; got != typed {
		t.Fatalf("endpoint = %q, want %q", got, typed)
	}
}
