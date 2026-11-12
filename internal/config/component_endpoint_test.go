package config

import (
	"testing"

	"github.com/spf13/cobra"
)

const (
	customEndpoint    = "https://api2.itg.rhombussystems.com"
	profileComponent  = "https://component.profile.example"
	envComponent      = "https://component.env.example"
	flagComponent     = "https://component.flag.example"
	otherCustomTarget = "http://localhost:8451"
)

// rootWithFlags returns a root command carrying the CLI's global flags, parsed from args.
func rootWithFlags(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "rhombus"}
	pf := root.PersistentFlags()
	for _, name := range []string{"profile", "output", "api-key", "endpoint-url", "component-endpoint-url", "partner-org"} {
		pf.String(name, "", "")
	}
	pf.Bool("verbose", false, "")
	if err := pf.Parse(args); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestComponentEndpointResolution(t *testing.T) {
	type layer struct{ endpoint, component string }
	for _, tc := range []struct {
		name               string
		profile, env, flag layer
		region             string // region stored at login
		wantEndpoint       string
		wantComponent      string
	}{
		// Region default, from the main endpoint's region.
		{name: "US default", wantEndpoint: DefaultEndpointURL, wantComponent: DefaultComponentEndpointURL},
		{name: "EU profile endpoint", profile: layer{endpoint: EUEndpointURL},
			wantEndpoint: EUEndpointURL, wantComponent: EUComponentEndpointURL},
		{name: "EU env endpoint", env: layer{endpoint: EUEndpointURL},
			wantEndpoint: EUEndpointURL, wantComponent: EUComponentEndpointURL},
		{name: "EU flag endpoint", flag: layer{endpoint: EUEndpointURL},
			wantEndpoint: EUEndpointURL, wantComponent: EUComponentEndpointURL},
		{name: "US flag endpoint over EU profile", profile: layer{endpoint: EUEndpointURL}, flag: layer{endpoint: DefaultEndpointURL},
			wantEndpoint: DefaultEndpointURL, wantComponent: DefaultComponentEndpointURL},
		{name: "EU flag endpoint over custom profile", profile: layer{endpoint: customEndpoint}, flag: layer{endpoint: EUEndpointURL},
			wantEndpoint: EUEndpointURL, wantComponent: EUComponentEndpointURL},
		{name: "explicit default endpoint is not custom", flag: layer{endpoint: DefaultEndpointURL},
			wantEndpoint: DefaultEndpointURL, wantComponent: DefaultComponentEndpointURL},

		// Explicit component endpoint: flag > env > profile > region default.
		{name: "profile component", profile: layer{component: profileComponent},
			wantEndpoint: DefaultEndpointURL, wantComponent: profileComponent},
		{name: "env component", env: layer{component: envComponent},
			wantEndpoint: DefaultEndpointURL, wantComponent: envComponent},
		{name: "flag component", flag: layer{component: flagComponent},
			wantEndpoint: DefaultEndpointURL, wantComponent: flagComponent},
		{name: "env beats profile", profile: layer{component: profileComponent}, env: layer{component: envComponent},
			wantEndpoint: DefaultEndpointURL, wantComponent: envComponent},
		{name: "flag beats env and profile", profile: layer{component: profileComponent}, env: layer{component: envComponent},
			flag: layer{component: flagComponent}, wantEndpoint: DefaultEndpointURL, wantComponent: flagComponent},
		{name: "explicit component beats EU region default", profile: layer{endpoint: EUEndpointURL, component: profileComponent},
			wantEndpoint: EUEndpointURL, wantComponent: profileComponent},

		// A custom main endpoint with no component endpoint keeps component calls on it.
		{name: "custom profile endpoint", profile: layer{endpoint: customEndpoint},
			wantEndpoint: customEndpoint, wantComponent: ""},
		{name: "custom env endpoint", env: layer{endpoint: customEndpoint},
			wantEndpoint: customEndpoint, wantComponent: ""},
		{name: "custom flag endpoint", flag: layer{endpoint: customEndpoint},
			wantEndpoint: customEndpoint, wantComponent: ""},
		{name: "custom flag endpoint over EU profile", profile: layer{endpoint: EUEndpointURL}, flag: layer{endpoint: customEndpoint},
			wantEndpoint: customEndpoint, wantComponent: ""},
		{name: "custom endpoint ignores stored region", profile: layer{endpoint: otherCustomTarget}, region: RegionEU,
			wantEndpoint: otherCustomTarget, wantComponent: ""},
		{name: "trailing slash counts as custom", flag: layer{endpoint: DefaultEndpointURL + "/"},
			wantEndpoint: DefaultEndpointURL + "/", wantComponent: ""},

		// ...unless a component endpoint is set too, at any layer.
		{name: "custom endpoint with profile component", flag: layer{endpoint: customEndpoint}, profile: layer{component: profileComponent},
			wantEndpoint: customEndpoint, wantComponent: profileComponent},
		{name: "custom endpoint with env component", profile: layer{endpoint: customEndpoint}, env: layer{component: envComponent},
			wantEndpoint: customEndpoint, wantComponent: envComponent},
		{name: "custom endpoint with flag component", env: layer{endpoint: customEndpoint}, flag: layer{component: flagComponent},
			wantEndpoint: customEndpoint, wantComponent: flagComponent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHome(t)
			fields := map[string]string{}
			if tc.profile.endpoint != "" {
				fields["endpoint_url"] = tc.profile.endpoint
			}
			if tc.profile.component != "" {
				fields["component_endpoint_url"] = tc.profile.component
			}
			if tc.region != "" {
				fields["region"] = tc.region
			}
			if err := saveConfigFields(DefaultProfile, fields); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RHOMBUS_ENDPOINT_URL", tc.env.endpoint)
			t.Setenv("RHOMBUS_COMPONENT_ENDPOINT_URL", tc.env.component)

			var args []string
			if tc.flag.endpoint != "" {
				args = append(args, "--endpoint-url", tc.flag.endpoint)
			}
			if tc.flag.component != "" {
				args = append(args, "--component-endpoint-url", tc.flag.component)
			}

			cfg := LoadFromCmd(rootWithFlags(t, args...))
			if cfg.EndpointURL != tc.wantEndpoint {
				t.Errorf("EndpointURL = %q, want %q", cfg.EndpointURL, tc.wantEndpoint)
			}
			if cfg.ComponentEndpointURL != tc.wantComponent {
				t.Errorf("ComponentEndpointURL = %q, want %q", cfg.ComponentEndpointURL, tc.wantComponent)
			}

			// LoadConfig (no flags) resolves the same way from the env and profile layers.
			if tc.flag == (layer{}) {
				if got := LoadConfig(DefaultProfile).ComponentEndpointURL; got != tc.wantComponent {
					t.Errorf("LoadConfig ComponentEndpointURL = %q, want %q", got, tc.wantComponent)
				}
			}
		})
	}
}

// A named profile reads component_endpoint_url from its own "profile <name>" section.
func TestComponentEndpointNamedProfile(t *testing.T) {
	setHome(t)
	if err := saveConfigFields("work", map[string]string{"endpoint_url": customEndpoint, "component_endpoint_url": profileComponent}); err != nil {
		t.Fatal(err)
	}
	if got := LoadConfig("work").ComponentEndpointURL; got != profileComponent {
		t.Errorf("work ComponentEndpointURL = %q, want %q", got, profileComponent)
	}
	if got := LoadConfig(DefaultProfile).ComponentEndpointURL; got != DefaultComponentEndpointURL {
		t.Errorf("default ComponentEndpointURL = %q, want %q", got, DefaultComponentEndpointURL)
	}
}

func TestComponentEndpointForRegion(t *testing.T) {
	for _, tc := range []struct{ region, want string }{
		{RegionUS, "https://api-component.rhombussystems.com"},
		{RegionEU, "https://api-component.eu.rhombussystems.com"},
		{"", "https://api-component.rhombussystems.com"},
		{"ap", "https://api-component.rhombussystems.com"},
	} {
		if got := ComponentEndpointForRegion(tc.region); got != tc.want {
			t.Errorf("ComponentEndpointForRegion(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
}
