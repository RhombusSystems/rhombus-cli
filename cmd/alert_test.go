package cmd

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

func TestMediaBaseURLForConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{
			name: "US token",
			cfg: config.Config{
				AuthType:    config.AuthTypeToken,
				EndpointURL: config.DefaultEndpointURL,
			},
			want: usTokenMediaBaseURL,
		},
		{
			name: "EU token",
			cfg: config.Config{
				AuthType:    config.AuthTypeToken,
				EndpointURL: config.EUEndpointURL,
			},
			want: euTokenMediaBaseURL,
		},
		{
			name: "US certificate",
			cfg: config.Config{
				AuthType:    config.AuthTypeCert,
				CertFile:    "client.crt",
				KeyFile:     "client.key",
				EndpointURL: config.DefaultEndpointURL,
			},
			want: usCertMediaBaseURL,
		},
		{
			name: "EU certificate",
			cfg: config.Config{
				AuthType:    config.AuthTypeCert,
				CertFile:    "client.crt",
				KeyFile:     "client.key",
				EndpointURL: config.EUEndpointURL,
			},
			want: euCertMediaBaseURL,
		},
		{
			name: "US OAuth",
			cfg: config.Config{
				AuthType:    config.AuthTypeOAuth,
				EndpointURL: config.DefaultEndpointURL,
			},
			want: usTokenMediaBaseURL,
		},
		{
			name: "EU OAuth",
			cfg: config.Config{
				AuthType:    config.AuthTypeOAuth,
				EndpointURL: config.EUEndpointURL,
			},
			want: euTokenMediaBaseURL,
		},
		{
			name: "incomplete certificate profile falls back to token host",
			cfg: config.Config{
				AuthType:    config.AuthTypeCert,
				CertFile:    "client.crt",
				EndpointURL: config.DefaultEndpointURL,
			},
			want: usTokenMediaBaseURL,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mediaBaseURLForConfig(test.cfg); got != test.want {
				t.Fatalf("mediaBaseURLForConfig() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMediaURIForAuth(t *testing.T) {
	const uri = "https://media.us-west-2.dash.rhombussystems.com/dash/api/x.mpd"
	cert := config.Config{AuthType: config.AuthTypeCert, CertFile: "c", KeyFile: "k"}
	if got := mediaURIForAuth(cert, uri); got != "https://media.us-west-2.dash-internal.rhombussystems.com/dash/api/x.mpd" {
		t.Errorf("cert profile URI = %q, want the dash-internal host", got)
	}
	for _, cfg := range []config.Config{{AuthType: config.AuthTypeToken}, {AuthType: config.AuthTypeOAuth}} {
		if got := mediaURIForAuth(cfg, uri); got != uri {
			t.Errorf("%s profile URI = %q, want it unchanged", cfg.AuthType, got)
		}
	}
}

// Media downloads carry the same auth headers as API calls: unchanged for API
// keys, the OAuth scheme + access token for OAuth profiles, and x-auth-org as a
// header for a client org. Request-specific headers (the LAN cookie) are kept.
func TestMediaDownloadAuthHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want map[string]string
	}{
		{"API key", config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1"},
			map[string]string{"X-Auth-Scheme": "api-token", "X-Auth-Apikey": "key-1", "X-Auth-Access-Token": "", "X-Auth-Org": ""}},
		{"partner API key with client org", config.Config{AuthType: config.AuthTypeToken, ApiKey: "key-1", IsPartner: true, PartnerOrg: "org-1"},
			map[string]string{"X-Auth-Scheme": "partner-api-token", "X-Auth-Apikey": "key-1", "X-Auth-Org": "org-1"}},
		{"OAuth", freshOAuthConfig(config.DefaultEndpointURL, false, ""),
			map[string]string{"X-Auth-Scheme": "api-oauth-token", "X-Auth-Access-Token": "access-1", "X-Auth-Apikey": ""}},
		{"partner OAuth with client org", freshOAuthConfig(config.DefaultEndpointURL, true, "org-1"),
			map[string]string{"X-Auth-Scheme": "partner-api-oauth-token", "X-Auth-Access-Token": "access-1", "X-Auth-Org": "org-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []http.Header
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, r.Header.Clone())
				w.Write([]byte("data"))
			}))
			defer srv.Close()

			dir := t.TempDir()
			if err := downloadWithAuthQuiet(tc.cfg, srv.URL+"/thumb.jpeg", filepath.Join(dir, "a")); err != nil {
				t.Fatal(err)
			}
			httpClient, err := client.GetMediaHTTPClient(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			cookie := func(req *http.Request) { req.Header.Set("Cookie", "RHOMBUS_SESSIONID=RFT:fed-1") }
			if err := downloadDashSegment(httpClient, srv.URL+"/seg_init.mp4", filepath.Join(dir, "b"), cookie); err != nil {
				t.Fatal(err)
			}

			for i, h := range seen {
				for k, v := range tc.want {
					if got := h.Get(k); got != v {
						t.Errorf("request %d: %s = %q, want %q", i, k, got, v)
					}
				}
			}
			if got := seen[1].Get("Cookie"); got != "RHOMBUS_SESSIONID=RFT:fed-1" {
				t.Errorf("LAN cookie = %q, want it kept", got)
			}
		})
	}
}
