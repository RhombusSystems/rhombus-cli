package config

import (
	"testing"

	"github.com/spf13/cobra"
)

const (
	customEndpoint    = "https://api2.itg.rhombussystems.com"
	profileOverride   = "https://service.profile.example"
	envOverride       = "https://service.env.example"
	flagOverride      = "https://service.flag.example"
	otherCustomTarget = "http://localhost:8451"
)

// The table itself, spelled out: hosts, path prefixes and override names are user-facing.
func TestServiceTable(t *testing.T) {
	want := []Service{
		{
			Name:       "component",
			Prefixes:   []string{"/api/component"},
			USEndpoint: "https://api-component.rhombussystems.com",
			EUEndpoint: "https://api-component.eu.rhombussystems.com",
			Flag:       "component-endpoint-url",
			EnvVar:     "RHOMBUS_COMPONENT_ENDPOINT_URL",
			ProfileKey: "component_endpoint_url",
		},
		{
			Name:       "search",
			Prefixes:   []string{"/api/search", "/api/eventSearchV2"},
			USEndpoint: "https://api-search.rhombussystems.com",
			EUEndpoint: "https://api-search.eu.rhombussystems.com",
			Flag:       "search-endpoint-url",
			EnvVar:     "RHOMBUS_SEARCH_ENDPOINT_URL",
			ProfileKey: "search_endpoint_url",
		},
		{
			Name:       "robot",
			Prefixes:   []string{"/api/robot"},
			USEndpoint: "https://api-robot.rhombussystems.com",
			EUEndpoint: "https://api-robot.eu.rhombussystems.com",
			Flag:       "robot-endpoint-url",
			EnvVar:     "RHOMBUS_ROBOT_ENDPOINT_URL",
			ProfileKey: "robot_endpoint_url",
		},
	}
	if len(Services) != len(want) {
		t.Fatalf("len(Services) = %d, want %d", len(Services), len(want))
	}
	for i, w := range want {
		got := Services[i]
		if got.Name != w.Name || got.USEndpoint != w.USEndpoint || got.EUEndpoint != w.EUEndpoint ||
			got.Flag != w.Flag || got.EnvVar != w.EnvVar || got.ProfileKey != w.ProfileKey ||
			len(got.Prefixes) != len(w.Prefixes) {
			t.Errorf("Services[%d] = %+v, want %+v", i, got, w)
			continue
		}
		for j := range w.Prefixes {
			if got.Prefixes[j] != w.Prefixes[j] {
				t.Errorf("Services[%d].Prefixes = %v, want %v", i, got.Prefixes, w.Prefixes)
			}
		}
	}
}

func TestServiceEndpointForRegion(t *testing.T) {
	for _, tc := range []struct {
		svc          Service
		region, want string
	}{
		{ComponentService, RegionUS, "https://api-component.rhombussystems.com"},
		{ComponentService, RegionEU, "https://api-component.eu.rhombussystems.com"},
		{ComponentService, "", "https://api-component.rhombussystems.com"},
		{ComponentService, "ap", "https://api-component.rhombussystems.com"},
		{SearchService, RegionUS, "https://api-search.rhombussystems.com"},
		{SearchService, RegionEU, "https://api-search.eu.rhombussystems.com"},
		{SearchService, "", "https://api-search.rhombussystems.com"},
		{SearchService, "ap", "https://api-search.rhombussystems.com"},
		{RobotService, RegionUS, "https://api-robot.rhombussystems.com"},
		{RobotService, RegionEU, "https://api-robot.eu.rhombussystems.com"},
		{RobotService, "", "https://api-robot.rhombussystems.com"},
		{RobotService, "ap", "https://api-robot.rhombussystems.com"},
	} {
		if got := tc.svc.EndpointForRegion(tc.region); got != tc.want {
			t.Errorf("%s.EndpointForRegion(%q) = %q, want %q", tc.svc.Name, tc.region, got, tc.want)
		}
	}
}

// Paths match a service on whole path segments only, and each path belongs to at most one service.
func TestServiceServes(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string // service name, "" for the main endpoint
	}{
		{"/api/component/createAccessControlledDoor", "component"},
		{"/api/component/elevator/findAccessControlledElevatorLandingShadows", "component"},
		{"/api/component", "component"},
		{"/api/componentEvents/x", ""},
		{"/api/Component/x", ""},
		{"/api/event/component/x", ""},
		{"/api/doorbellcamera/findComponentEventsForDoorbellCamera", ""},

		{"/api/search/searchLicensePlates", "search"},
		{"/api/search", "search"},
		{"/api/eventSearchV2/searchIntegrationAccessEvents", "search"},
		{"/api/eventSearchV2", "search"},
		{"/api/searchFoo/x", ""},
		{"/api/searchFoo", ""},
		{"/api/eventSearch/getEventSeekpoints", ""},
		{"/api/eventSearchV2Foo/x", ""},
		{"/api/eventSearchV3/x", ""},
		{"/api/Search/x", ""},
		{"/api/camera/search/x", ""},

		{"/api/robot/getConfig", "robot"},
		{"/api/robot", "robot"},
		{"/api/robots/x", ""},
		{"/api/robotics", ""},
		{"/api/Robot/getConfig", ""},
		{"/api/camera/robot/x", ""},

		{"/api/camera/getMinimalCameraStateList", ""},
		{"", ""},
	} {
		var served []string
		for _, s := range Services {
			if s.Serves(tc.path) {
				served = append(served, s.Name)
			}
		}
		switch {
		case tc.want == "" && len(served) != 0:
			t.Errorf("%q served by %v, want main endpoint", tc.path, served)
		case tc.want != "" && (len(served) != 1 || served[0] != tc.want):
			t.Errorf("%q served by %v, want only %s", tc.path, served, tc.want)
		}
	}
}

// rootWithFlags returns a root command carrying the CLI's global flags, parsed from args.
func rootWithFlags(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "rhombus"}
	pf := root.PersistentFlags()
	for _, name := range []string{"profile", "output", "api-key", "endpoint-url", "partner-org"} {
		pf.String(name, "", "")
	}
	for _, svc := range Services {
		pf.String(svc.Flag, "", "")
	}
	pf.Bool("verbose", false, "")
	if err := pf.Parse(args); err != nil {
		t.Fatal(err)
	}
	return root
}

// Each service's endpoint resolves the same way, independently of the others: its own override (flag > env >
// profile), else its host for the main endpoint's region, else none for a custom main endpoint.
func TestServiceEndpointResolution(t *testing.T) {
	type layer struct{ endpoint, override string }
	cases := []struct {
		name               string
		profile, env, flag layer
		region             string // region stored at login
		wantEndpoint       string
		wantRegion         string // region of wantEndpoint, "" when custom
		wantOverride       string // the service's endpoint when overridden, else its region default (or none)
	}{
		// Region default, from the main endpoint's region.
		{name: "US default", wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS},
		{name: "EU profile endpoint", profile: layer{endpoint: EUEndpointURL}, wantEndpoint: EUEndpointURL, wantRegion: RegionEU},
		{name: "EU env endpoint", env: layer{endpoint: EUEndpointURL}, wantEndpoint: EUEndpointURL, wantRegion: RegionEU},
		{name: "EU flag endpoint", flag: layer{endpoint: EUEndpointURL}, wantEndpoint: EUEndpointURL, wantRegion: RegionEU},
		{name: "US flag endpoint over EU profile", profile: layer{endpoint: EUEndpointURL}, flag: layer{endpoint: DefaultEndpointURL},
			wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS},
		{name: "EU flag endpoint over custom profile", profile: layer{endpoint: customEndpoint}, flag: layer{endpoint: EUEndpointURL},
			wantEndpoint: EUEndpointURL, wantRegion: RegionEU},
		{name: "explicit default endpoint is not custom", flag: layer{endpoint: DefaultEndpointURL},
			wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS},

		// Explicit service endpoint: flag > env > profile > region default.
		{name: "profile override", profile: layer{override: profileOverride},
			wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS, wantOverride: profileOverride},
		{name: "env override", env: layer{override: envOverride},
			wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS, wantOverride: envOverride},
		{name: "flag override", flag: layer{override: flagOverride},
			wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS, wantOverride: flagOverride},
		{name: "env beats profile", profile: layer{override: profileOverride}, env: layer{override: envOverride},
			wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS, wantOverride: envOverride},
		{name: "flag beats env and profile", profile: layer{override: profileOverride}, env: layer{override: envOverride},
			flag: layer{override: flagOverride}, wantEndpoint: DefaultEndpointURL, wantRegion: RegionUS, wantOverride: flagOverride},
		{name: "override beats EU region default", profile: layer{endpoint: EUEndpointURL, override: profileOverride},
			wantEndpoint: EUEndpointURL, wantRegion: RegionEU, wantOverride: profileOverride},

		// A custom main endpoint with no service endpoint keeps the service's calls on it.
		{name: "custom profile endpoint", profile: layer{endpoint: customEndpoint}, wantEndpoint: customEndpoint},
		{name: "custom env endpoint", env: layer{endpoint: customEndpoint}, wantEndpoint: customEndpoint},
		{name: "custom flag endpoint", flag: layer{endpoint: customEndpoint}, wantEndpoint: customEndpoint},
		{name: "custom flag endpoint over EU profile", profile: layer{endpoint: EUEndpointURL}, flag: layer{endpoint: customEndpoint},
			wantEndpoint: customEndpoint},
		{name: "custom endpoint ignores stored region", profile: layer{endpoint: otherCustomTarget}, region: RegionEU,
			wantEndpoint: otherCustomTarget},
		{name: "trailing slash counts as custom", flag: layer{endpoint: DefaultEndpointURL + "/"},
			wantEndpoint: DefaultEndpointURL + "/"},

		// ...unless the service's endpoint is set too, at any layer.
		{name: "custom endpoint with profile override", flag: layer{endpoint: customEndpoint}, profile: layer{override: profileOverride},
			wantEndpoint: customEndpoint, wantOverride: profileOverride},
		{name: "custom endpoint with env override", profile: layer{endpoint: customEndpoint}, env: layer{override: envOverride},
			wantEndpoint: customEndpoint, wantOverride: envOverride},
		{name: "custom endpoint with flag override", env: layer{endpoint: customEndpoint}, flag: layer{override: flagOverride},
			wantEndpoint: customEndpoint, wantOverride: flagOverride},
	}

	for _, svc := range Services {
		for _, tc := range cases {
			t.Run(svc.Name+"/"+tc.name, func(t *testing.T) {
				setHome(t)
				fields := map[string]string{}
				if tc.profile.endpoint != "" {
					fields["endpoint_url"] = tc.profile.endpoint
				}
				if tc.profile.override != "" {
					fields[svc.ProfileKey] = tc.profile.override
				}
				if tc.region != "" {
					fields["region"] = tc.region
				}
				if err := saveConfigFields(DefaultProfile, fields); err != nil {
					t.Fatal(err)
				}
				t.Setenv("RHOMBUS_ENDPOINT_URL", tc.env.endpoint)
				t.Setenv(svc.EnvVar, tc.env.override)

				var args []string
				if tc.flag.endpoint != "" {
					args = append(args, "--endpoint-url", tc.flag.endpoint)
				}
				if tc.flag.override != "" {
					args = append(args, "--"+svc.Flag, tc.flag.override)
				}

				// want is what every service should resolve to: the override for svc, the region default (or
				// nothing for a custom main endpoint) for svc without one and for every other service.
				want := func(s Service) string {
					if s.Name == svc.Name && tc.wantOverride != "" {
						return tc.wantOverride
					}
					if tc.wantRegion == "" {
						return ""
					}
					return s.EndpointForRegion(tc.wantRegion)
				}
				check := func(from string, cfg Config) {
					t.Helper()
					if cfg.EndpointURL != tc.wantEndpoint {
						t.Errorf("%s EndpointURL = %q, want %q", from, cfg.EndpointURL, tc.wantEndpoint)
					}
					for _, s := range Services {
						if got := cfg.ServiceEndpoints[s.Name]; got != want(s) {
							t.Errorf("%s ServiceEndpoints[%s] = %q, want %q", from, s.Name, got, want(s))
						}
					}
				}

				check("LoadFromCmd", LoadFromCmd(rootWithFlags(t, args...)))
				// LoadConfig (no flags) resolves the same way from the env and profile layers.
				if tc.flag == (layer{}) {
					check("LoadConfig", LoadConfig(DefaultProfile))
				}
			})
		}
	}
}

// A named profile reads each service's key from its own "profile <name>" section.
func TestServiceEndpointNamedProfile(t *testing.T) {
	for _, svc := range Services {
		t.Run(svc.Name, func(t *testing.T) {
			setHome(t)
			if err := saveConfigFields("work", map[string]string{"endpoint_url": customEndpoint, svc.ProfileKey: profileOverride}); err != nil {
				t.Fatal(err)
			}
			work := LoadConfig("work")
			for _, s := range Services {
				want := ""
				if s.Name == svc.Name {
					want = profileOverride
				}
				if got := work.ServiceEndpoints[s.Name]; got != want {
					t.Errorf("work ServiceEndpoints[%s] = %q, want %q", s.Name, got, want)
				}
			}
			for _, s := range Services {
				if got := LoadConfig(DefaultProfile).ServiceEndpoints[s.Name]; got != s.USEndpoint {
					t.Errorf("default ServiceEndpoints[%s] = %q, want %q", s.Name, got, s.USEndpoint)
				}
			}
		})
	}
}

func TestServiceEndpointFor(t *testing.T) {
	cfg := Config{ServiceEndpoints: map[string]string{"component": "https://c.example", "search": "https://s.example"}}
	for _, tc := range []struct{ path, want string }{
		{"/api/component/findAccessControlledDoors", "https://c.example"},
		{"/api/search/searchLicensePlates", "https://s.example"},
		{"/api/eventSearchV2/searchIntegrationAccessEvents", "https://s.example"},
		{"/api/robot/getConfig", ""}, // no robot endpoint resolved
		{"/api/camera/getMinimalCameraStateList", ""},
	} {
		if got := cfg.ServiceEndpointFor(tc.path); got != tc.want {
			t.Errorf("ServiceEndpointFor(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := (Config{}).ServiceEndpointFor("/api/component/x"); got != "" {
		t.Errorf("zero Config ServiceEndpointFor = %q, want empty", got)
	}
}

func TestIsServiceFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"component-endpoint-url", true},
		{"search-endpoint-url", true},
		{"robot-endpoint-url", true},
		{"endpoint-url", false},
		{"license-plate", false},
	} {
		if got := IsServiceFlag(tc.name); got != tc.want {
			t.Errorf("IsServiceFlag(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
