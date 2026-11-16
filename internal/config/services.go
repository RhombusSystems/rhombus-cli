package config

import (
	"strings"
)

// Service is an API service that runs apart from the main API and has its own
// public host per region. The CLI calls those hosts directly: a call to a path
// the service serves goes to Config.ServiceEndpoints[Name] rather than to
// EndpointURL (see Config.ServiceEndpointFor).
type Service struct {
	Name string // short name: the key in Config.ServiceEndpoints

	// Prefixes are the API paths the service serves, each matched on whole path
	// segments: "/api/search" covers "/api/search" and "/api/search/x" but not
	// "/api/searchFoo".
	Prefixes []string

	USEndpoint string // the service's public host in the US region (and unknown regions)
	EUEndpoint string // the service's public host in the EU region

	// Explicit overrides of the host, in order of precedence.
	Flag       string // global flag
	EnvVar     string // environment variable
	ProfileKey string // key in the profile's section of ~/.rhombus/config
}

var (
	ComponentService = Service{
		Name:       "component",
		Prefixes:   []string{"/api/component"},
		USEndpoint: "https://api-component.rhombussystems.com",
		EUEndpoint: "https://api-component.eu.rhombussystems.com",
		Flag:       "component-endpoint-url",
		EnvVar:     "RHOMBUS_COMPONENT_ENDPOINT_URL",
		ProfileKey: "component_endpoint_url",
	}
	SearchService = Service{
		Name:       "search",
		Prefixes:   []string{"/api/search", "/api/eventSearchV2"},
		USEndpoint: "https://api-search.rhombussystems.com",
		EUEndpoint: "https://api-search.eu.rhombussystems.com",
		Flag:       "search-endpoint-url",
		EnvVar:     "RHOMBUS_SEARCH_ENDPOINT_URL",
		ProfileKey: "search_endpoint_url",
	}
	RobotService = Service{
		Name:       "robot",
		Prefixes:   []string{"/api/robot"},
		USEndpoint: "https://api-robot.rhombussystems.com",
		EUEndpoint: "https://api-robot.eu.rhombussystems.com",
		Flag:       "robot-endpoint-url",
		EnvVar:     "RHOMBUS_ROBOT_ENDPOINT_URL",
		ProfileKey: "robot_endpoint_url",
	}

	// Services lists every service the CLI calls at its own host.
	Services = []Service{ComponentService, SearchService, RobotService}
)

// Serves reports whether path is one of the service's paths.
func (s Service) Serves(path string) bool {
	for _, p := range s.Prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// EndpointForRegion returns the service's public host for the given region.
// Unknown regions fall back to the US host.
func (s Service) EndpointForRegion(region string) string {
	if region == RegionEU {
		return s.EUEndpoint
	}
	return s.USEndpoint
}

// FlagUsage is the help text of the service's global flag.
func (s Service) FlagUsage() string {
	return "Override endpoint URL for " + s.pathPatterns() + " calls (default: the region's " + s.Name + " host)"
}

// pathPatterns renders Prefixes for help text, e.g. "/api/search* and /api/eventSearchV2*".
func (s Service) pathPatterns() string {
	patterns := make([]string, len(s.Prefixes))
	for i, p := range s.Prefixes {
		patterns[i] = p + "*"
	}
	return strings.Join(patterns, " and ")
}

// IsServiceFlag reports whether name is one of the services' global endpoint flags.
func IsServiceFlag(name string) bool {
	for _, s := range Services {
		if s.Flag == name {
			return true
		}
	}
	return false
}

// ServiceEndpointFor returns the host for a call to path when a service in
// Services serves it and has a resolved endpoint, or "" when the call goes to
// EndpointURL.
func (c Config) ServiceEndpointFor(path string) string {
	for _, s := range Services {
		if s.Serves(path) {
			return c.ServiceEndpoints[s.Name]
		}
	}
	return ""
}

// setServiceOverride records an explicitly configured endpoint for s. Empty
// values are ignored, so a lower-precedence override still applies.
func (c *Config) setServiceOverride(s Service, endpoint string) {
	if endpoint == "" {
		return
	}
	if c.serviceOverrides == nil {
		c.serviceOverrides = make(map[string]string, len(Services))
	}
	c.serviceOverrides[s.Name] = endpoint
}

// resolveServiceEndpoints sets ServiceEndpoints. Each service gets its explicit
// override if there is one; otherwise its host for the main endpoint's region,
// or no entry when the main endpoint is custom (not one of the regional
// defaults): a custom deployment's calls stay on that deployment.
func (c *Config) resolveServiceEndpoints() {
	region := RegionForEndpoint(c.EndpointURL)
	c.ServiceEndpoints = make(map[string]string, len(Services))
	for _, s := range Services {
		switch {
		case c.serviceOverrides[s.Name] != "":
			c.ServiceEndpoints[s.Name] = c.serviceOverrides[s.Name]
		case region != "":
			c.ServiceEndpoints[s.Name] = s.EndpointForRegion(region)
		}
	}
}
