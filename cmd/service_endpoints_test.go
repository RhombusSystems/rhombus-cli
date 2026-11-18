package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Commands calling a service with its own host (component, search, robot, guest) go straight to that host through the root
// command's flags and env vars, with no change to the generated code; other commands, and service commands under a
// custom --endpoint-url with no service endpoint, stay on the main endpoint. The global flags are never sent in the
// request body.
func TestCommandsUseServiceEndpoints(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	serviceEnv := map[string]string{
		"component": "RHOMBUS_COMPONENT_ENDPOINT_URL",
		"search":    "RHOMBUS_SEARCH_ENDPOINT_URL",
		"robot":     "RHOMBUS_ROBOT_ENDPOINT_URL",
		"guest":     "RHOMBUS_GUEST_ENDPOINT_URL",
	}
	for _, env := range []string{"RHOMBUS_API_KEY", "RHOMBUS_PROFILE", "RHOMBUS_OUTPUT", "RHOMBUS_ENDPOINT_URL", "CLAUDECODE"} {
		t.Setenv(env, "")
	}
	for _, env := range serviceEnv {
		t.Setenv(env, "")
	}
	// rootCmd is shared and keeps flag values between Execute calls, so each run clears the flags it set. Clear them
	// only after a run: setting a persistent flag before the first Execute hides it from params.CollectFlags, which
	// would mask a global flag leaking into the request body.
	resetFlags := func() {
		for _, f := range []string{"api-key", "endpoint-url", "component-endpoint-url", "search-endpoint-url", "robot-endpoint-url",
			"guest-endpoint-url"} {
			_ = rootCmd.PersistentFlags().Set(f, "")
		}
	}
	t.Cleanup(resetFlags)

	type hit struct {
		host, path string
		body       map[string]any
	}
	var mu sync.Mutex
	var hits []hit
	server := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			hits = append(hits, hit{name, r.URL.Path, body})
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		}))
	}
	mainSrv, componentSrv, searchSrv, robotSrv, guestSrv := server("main"), server("component"), server("search"),
		server("robot"), server("guest")
	for _, s := range []*httptest.Server{mainSrv, componentSrv, searchSrv, robotSrv, guestSrv} {
		defer s.Close()
	}
	serviceURL := map[string]string{"component": componentSrv.URL, "search": searchSrv.URL, "robot": robotSrv.URL,
		"guest": guestSrv.URL}

	const (
		doorLabel    = "/api/component/addAccessControlledDoorLabel"
		elevator     = "/api/component/elevator/findAccessControlledElevatorLandingShadows"
		plates       = "/api/search/searchLicensePlates"
		accessEvents = "/api/eventSearchV2/searchIntegrationAccessEvents"
		v1Seekpoints = "/api/eventSearch/getCameraOrDoorbellCameraSeekpoints"
		robotConfig  = "/api/robot/getConfig"
		kiosk        = "/api/kiosk/updateKiosk"
		cameras      = "/api/camera/getMinimalCameraStateList"
	)
	doorCmd := []string{"component", "add-access-controlled-door-label"}
	elevatorCmd := []string{"elevator", "find-access-controlled-elevator-landing-shadows"}
	platesCmd := []string{"search", "search-license-plates"}
	accessEventsCmd := []string{"elements", "events"} // hand-written command on /api/eventSearchV2
	v1SeekpointsCmd := []string{"event-search", "get-camera-or-doorbell-camera-seekpoints"}
	robotCmd := []string{"robot", "get-robot-config"}
	kioskCmd := []string{"guest-management-kiosk", "update-kiosk"}
	cameraCmd := []string{"camera", "get-minimal-camera-state-list"}

	withFlags := []string{"--api-key", "k", "--endpoint-url", mainSrv.URL, "--component-endpoint-url", componentSrv.URL,
		"--search-endpoint-url", searchSrv.URL, "--robot-endpoint-url", robotSrv.URL, "--guest-endpoint-url", guestSrv.URL}
	customOnly := []string{"--api-key", "k", "--endpoint-url", mainSrv.URL}

	for _, tc := range []struct {
		name     string
		command  []string
		flags    []string
		env      string // service whose env var points at its server
		wantHost string
		wantPath string
	}{
		{"component command, flag", doorCmd, withFlags, "", "component", doorLabel},
		{"elevator command, flag", elevatorCmd, withFlags, "", "component", elevator},
		{"search command, flag", platesCmd, withFlags, "", "search", plates},
		{"eventSearchV2 command, flag", accessEventsCmd, withFlags, "", "search", accessEvents},
		{"robot command, flag", robotCmd, withFlags, "", "robot", robotConfig},
		{"kiosk command, flag", kioskCmd, withFlags, "", "guest", kiosk},
		{"non-service command", cameraCmd, withFlags, "", "main", cameras},
		{"eventSearch v1 command", v1SeekpointsCmd, withFlags, "", "main", v1Seekpoints},

		{"component command, env var", doorCmd, customOnly, "component", "component", doorLabel},
		{"search command, env var", platesCmd, customOnly, "search", "search", plates},
		{"eventSearchV2 command, env var", accessEventsCmd, customOnly, "search", "search", accessEvents},
		{"robot command, env var", robotCmd, customOnly, "robot", "robot", robotConfig},
		{"kiosk command, env var", kioskCmd, customOnly, "guest", "guest", kiosk},
		// One service's env var does not move another service's calls.
		{"robot env var, search command", platesCmd, customOnly, "robot", "main", plates},
		{"search env var, component command", doorCmd, customOnly, "search", "main", doorLabel},
		{"guest env var, robot command", robotCmd, customOnly, "guest", "main", robotConfig},

		{"custom endpoint, no component endpoint", doorCmd, customOnly, "", "main", doorLabel},
		{"custom endpoint, no component endpoint, elevator", elevatorCmd, customOnly, "", "main", elevator},
		{"custom endpoint, no search endpoint", platesCmd, customOnly, "", "main", plates},
		{"custom endpoint, no search endpoint, eventSearchV2", accessEventsCmd, customOnly, "", "main", accessEvents},
		{"custom endpoint, no robot endpoint", robotCmd, customOnly, "", "main", robotConfig},
		{"custom endpoint, no guest endpoint", kioskCmd, customOnly, "", "main", kiosk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer resetFlags()
			for svc, env := range serviceEnv {
				v := ""
				if svc == tc.env {
					v = serviceURL[svc]
				}
				t.Setenv(env, v)
			}
			mu.Lock()
			hits = nil
			mu.Unlock()

			rootCmd.SetArgs(append(append([]string{}, tc.command...), tc.flags...))
			rootCmd.SetOut(io.Discard)
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("rhombus %v: %v", tc.command, err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(hits) != 1 || hits[0].host != tc.wantHost || hits[0].path != tc.wantPath {
				t.Fatalf("requests = %+v, want one to %s%s", hits, tc.wantHost, tc.wantPath)
			}
			for _, k := range []string{"componentEndpointUrl", "searchEndpointUrl", "robotEndpointUrl", "guestEndpointUrl", "endpointUrl",
				"apiKey"} {
				if _, ok := hits[0].body[k]; ok {
					t.Errorf("request body carries global flag %q: %v", k, hits[0].body)
				}
			}
		})
	}
}
