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
)

func TestBaseURLRoutesComponentCalls(t *testing.T) {
	token := config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: mainHost, ComponentEndpointURL: componentHost}
	oauth := config.Config{AuthType: config.AuthTypeOAuth, OAuthAccessToken: "a", EndpointURL: mainHost, ComponentEndpointURL: componentHost}
	cert := config.Config{AuthType: config.AuthTypeCert, ApiKey: "k", CertFile: "/c.crt", KeyFile: "/c.key",
		EndpointURL: mainHost, ComponentEndpointURL: componentHost}
	// auth_type=cert without cert/key files presents no certificate (it authenticates with the API key).
	certTypeNoFiles := config.Config{AuthType: config.AuthTypeCert, ApiKey: "k", EndpointURL: mainHost, ComponentEndpointURL: componentHost}
	noComponent := config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: mainHost}

	for _, tc := range []struct {
		name string
		cfg  config.Config
		path string
		want string
	}{
		{"component call", token, "/api/component/createAccessControlledDoor", componentHost},
		{"elevator component call", token, "/api/component/elevator/findAccessControlledElevatorLandingShadows", componentHost},
		{"component call with OAuth", oauth, "/api/component/findAccessControlledDoors", componentHost},
		{"non-component call", token, "/api/camera/getMinimalCameraStateList", mainHost},
		{"component segment not at the start", token, "/api/event/component/x", mainHost},
		{"case-sensitive prefix", token, "/api/Component/x", mainHost},
		{"no component endpoint", noComponent, "/api/component/createAccessControlledDoor", mainHost},
		{"client certificate", cert, "/api/component/createAccessControlledDoor", mainHost},
		{"client certificate, elevator", cert, "/api/component/elevator/findAccessControlledElevatorLandingShadows", mainHost},
		{"cert auth type without cert files", certTypeNoFiles, "/api/component/createAccessControlledDoor", componentHost},
		{"client certificate, non-component", cert, "/api/camera/getMinimalCameraStateList", mainHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := baseURL(tc.cfg, tc.path); got != tc.want {
				t.Errorf("baseURL(%s) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// APICall sends each request to the host baseURL picks, with the same path and credentials.
func TestAPICallSendsComponentCallsToComponentHost(t *testing.T) {
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
	mainSrv, componentSrv := server("main"), server("component")
	defer mainSrv.Close()
	defer componentSrv.Close()

	direct := config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", EndpointURL: mainSrv.URL, ComponentEndpointURL: componentSrv.URL}
	fallback := config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", EndpointURL: mainSrv.URL}

	for _, tc := range []struct {
		name     string
		cfg      config.Config
		path     string
		wantHost string
	}{
		{"component call", direct, "/api/component/findAccessControlledDoors", "component"},
		{"elevator call", direct, "/api/component/elevator/findAccessControlledElevatorLandingShadows", "component"},
		{"non-component call", direct, "/api/camera/getMinimalCameraStateList", "main"},
		{"component call without component endpoint", fallback, "/api/component/findAccessControlledDoors", "main"},
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
