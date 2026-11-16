package client

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

const (
	mainHost      = "https://api2.example"
	componentHost = "https://api-component.example"
	searchHost    = "https://api-search.example"
	robotHost     = "https://api-robot.example"
)

func allServices() map[string]string {
	return map[string]string{"component": componentHost, "search": searchHost, "robot": robotHost}
}

func TestBaseURLRoutesServiceCalls(t *testing.T) {
	token := config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: mainHost, ServiceEndpoints: allServices()}
	oauth := config.Config{AuthType: config.AuthTypeOAuth, OAuthAccessToken: "a", EndpointURL: mainHost, ServiceEndpoints: allServices()}
	cert := config.Config{AuthType: config.AuthTypeCert, ApiKey: "k", CertFile: "/c.crt", KeyFile: "/c.key",
		EndpointURL: mainHost, ServiceEndpoints: allServices()}
	// auth_type=cert without cert/key files presents no certificate (it authenticates with the API key).
	certTypeNoFiles := config.Config{AuthType: config.AuthTypeCert, ApiKey: "k", EndpointURL: mainHost, ServiceEndpoints: allServices()}
	noServices := config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: mainHost}
	// Only search resolved, e.g. a custom main endpoint with --search-endpoint-url.
	searchOnly := config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: mainHost,
		ServiceEndpoints: map[string]string{"search": searchHost}}

	const (
		door          = "/api/component/createAccessControlledDoor"
		elevator      = "/api/component/elevator/findAccessControlledElevatorLandingShadows"
		plates        = "/api/search/searchLicensePlates"
		accessEvents  = "/api/eventSearchV2/searchIntegrationAccessEvents"
		robotConfig   = "/api/robot/getConfig"
		cameras       = "/api/camera/getMinimalCameraStateList"
		v1EventSearch = "/api/eventSearch/getEventSeekpoints"
	)

	for _, tc := range []struct {
		name string
		cfg  config.Config
		path string
		want string
	}{
		{"component call", token, door, componentHost},
		{"elevator component call", token, elevator, componentHost},
		{"component call with OAuth", oauth, "/api/component/findAccessControlledDoors", componentHost},
		{"search call", token, plates, searchHost},
		{"eventSearchV2 call", token, accessEvents, searchHost},
		{"search call with OAuth", oauth, accessEvents, searchHost},
		{"robot call", token, robotConfig, robotHost},
		{"robot call with OAuth", oauth, robotConfig, robotHost},
		{"non-service call", token, cameras, mainHost},
		{"eventSearch v1 stays on main", token, v1EventSearch, mainHost},

		// Look-alike paths stay on the main endpoint: the match is on whole path segments.
		{"component look-alike", token, "/api/componentEvents/x", mainHost},
		{"component segment not at the start", token, "/api/event/component/x", mainHost},
		{"case-sensitive component", token, "/api/Component/x", mainHost},
		{"search look-alike", token, "/api/searchFoo/x", mainHost},
		{"eventSearchV2 look-alike", token, "/api/eventSearchV2Foo/x", mainHost},
		{"case-sensitive search", token, "/api/Search/x", mainHost},
		{"robot look-alike", token, "/api/robots/x", mainHost},
		{"case-sensitive robot", token, "/api/Robot/getConfig", mainHost},

		// No endpoint resolved for the service: its calls stay on the main endpoint.
		{"no component endpoint", noServices, door, mainHost},
		{"no search endpoint", noServices, plates, mainHost},
		{"no eventSearchV2 endpoint", noServices, accessEvents, mainHost},
		{"no robot endpoint", noServices, robotConfig, mainHost},
		{"only search resolved: search", searchOnly, plates, searchHost},
		{"only search resolved: component", searchOnly, door, mainHost},
		{"only search resolved: robot", searchOnly, robotConfig, mainHost},

		// A client certificate keeps every call on the main endpoint.
		{"client certificate, component", cert, door, mainHost},
		{"client certificate, elevator", cert, elevator, mainHost},
		{"client certificate, search", cert, plates, mainHost},
		{"client certificate, eventSearchV2", cert, accessEvents, mainHost},
		{"client certificate, robot", cert, robotConfig, mainHost},
		{"client certificate, non-service", cert, cameras, mainHost},
		{"cert auth type without cert files, component", certTypeNoFiles, door, componentHost},
		{"cert auth type without cert files, search", certTypeNoFiles, plates, searchHost},
		{"cert auth type without cert files, robot", certTypeNoFiles, robotConfig, robotHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := baseURL(tc.cfg, tc.path); got != tc.want {
				t.Errorf("baseURL(%s) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// APICall sends each request to the host baseURL picks, with the same path and credentials.
func TestAPICallSendsServiceCallsToServiceHosts(t *testing.T) {
	type hit struct{ host, path, apiKey string }
	var mu sync.Mutex
	var hits []hit
	server := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits = append(hits, hit{name, r.URL.Path, r.Header.Get("x-auth-apikey")})
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		}))
	}
	mainSrv, componentSrv, searchSrv, robotSrv := server("main"), server("component"), server("search"), server("robot")
	for _, s := range []*httptest.Server{mainSrv, componentSrv, searchSrv, robotSrv} {
		defer s.Close()
	}

	direct := config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", EndpointURL: mainSrv.URL,
		ServiceEndpoints: map[string]string{"component": componentSrv.URL, "search": searchSrv.URL, "robot": robotSrv.URL}}
	fallback := config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", EndpointURL: mainSrv.URL}

	for _, tc := range []struct {
		name     string
		cfg      config.Config
		path     string
		wantHost string
	}{
		{"component call", direct, "/api/component/findAccessControlledDoors", "component"},
		{"elevator call", direct, "/api/component/elevator/findAccessControlledElevatorLandingShadows", "component"},
		{"search call", direct, "/api/search/searchLicensePlates", "search"},
		{"eventSearchV2 call", direct, "/api/eventSearchV2/searchIntegrationAccessEvents", "search"},
		{"robot call", direct, "/api/robot/getConfig", "robot"},
		{"non-service call", direct, "/api/camera/getMinimalCameraStateList", "main"},
		{"component call without service endpoints", fallback, "/api/component/findAccessControlledDoors", "main"},
		{"search call without service endpoints", fallback, "/api/search/searchLicensePlates", "main"},
		{"robot call without service endpoints", fallback, "/api/robot/getConfig", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			hits = nil
			mu.Unlock()
			if _, err := APICall(tc.cfg, tc.path, map[string]any{}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := hit{tc.wantHost, tc.path, "key-1"}
			if len(hits) != 1 || hits[0] != want {
				t.Errorf("requests = %+v, want [%+v]", hits, want)
			}
		})
	}
}
