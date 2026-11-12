package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Generated /api/component* commands go straight to the component host through the root command's flags and env
// vars, with no change to the generated code; other commands, and component commands under a custom --endpoint-url
// with no component endpoint, stay on the main endpoint. The global flags are never sent in the request body.
func TestGeneratedComponentCommandsUseComponentEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, env := range []string{"RHOMBUS_API_KEY", "RHOMBUS_PROFILE", "RHOMBUS_OUTPUT", "RHOMBUS_ENDPOINT_URL",
		"RHOMBUS_COMPONENT_ENDPOINT_URL", "CLAUDECODE"} {
		t.Setenv(env, "")
	}
	// rootCmd is shared and keeps flag values between Execute calls, so each run clears the flags it set. Clear them
	// only after a run: setting a persistent flag before the first Execute hides it from params.CollectFlags, which
	// would mask a global flag leaking into the request body.
	resetFlags := func() {
		for _, f := range []string{"api-key", "endpoint-url", "component-endpoint-url"} {
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
	mainSrv, componentSrv := server("main"), server("component")
	defer mainSrv.Close()
	defer componentSrv.Close()

	const (
		doorLabel = "/api/component/addAccessControlledDoorLabel"
		elevator  = "/api/component/elevator/findAccessControlledElevatorLandingShadows"
		cameras   = "/api/camera/getMinimalCameraStateList"
	)
	doorCmd := []string{"component", "add-access-controlled-door-label"}
	elevatorCmd := []string{"elevator", "find-access-controlled-elevator-landing-shadows"}
	cameraCmd := []string{"camera", "get-minimal-camera-state-list"}

	withFlags := []string{"--api-key", "k", "--endpoint-url", mainSrv.URL, "--component-endpoint-url", componentSrv.URL}
	customOnly := []string{"--api-key", "k", "--endpoint-url", mainSrv.URL}

	for _, tc := range []struct {
		name     string
		command  []string
		flags    []string
		env      string // RHOMBUS_COMPONENT_ENDPOINT_URL
		wantHost string
		wantPath string
	}{
		{"component command, flag", doorCmd, withFlags, "", "component", doorLabel},
		{"elevator command, flag", elevatorCmd, withFlags, "", "component", elevator},
		{"non-component command", cameraCmd, withFlags, "", "main", cameras},
		{"component command, env var", doorCmd, customOnly, componentSrv.URL, "component", doorLabel},
		{"custom endpoint, no component endpoint", doorCmd, customOnly, "", "main", doorLabel},
		{"custom endpoint, no component endpoint, elevator", elevatorCmd, customOnly, "", "main", elevator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer resetFlags()
			t.Setenv("RHOMBUS_COMPONENT_ENDPOINT_URL", tc.env)
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
			for _, k := range []string{"componentEndpointUrl", "endpointUrl", "apiKey"} {
				if _, ok := hits[0].body[k]; ok {
					t.Errorf("request body carries global flag %q: %v", k, hits[0].body)
				}
			}
		})
	}
}
