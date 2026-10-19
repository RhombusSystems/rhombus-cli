package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

func freshOAuthConfig(endpoint string, partner bool, org string) config.Config {
	return config.Config{
		AuthType:          config.AuthTypeOAuth,
		EndpointURL:       endpoint,
		OAuthAccessToken:  "access-1",
		OAuthRefreshToken: "refresh-1",
		OAuthExpiresAt:    time.Now().Add(time.Hour).Unix(),
		IsPartner:         partner,
		PartnerOrg:        org,
	}
}

func TestBuildWSURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"US key", config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: config.DefaultEndpointURL},
			"wss://ws.rhombussystems.com:8443/websocket?x-auth-scheme=api-token"},
		{"EU key", config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: config.EUEndpointURL},
			"wss://ws.eu.rhombussystems.com:8443/websocket?x-auth-scheme=api-token"},
		{"staging endpoint", config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: itgEndpoint},
			"wss://ws.itg.rhombussystems.com:8443/websocket?x-auth-scheme=api-token"},
		{"custom endpoint with stored EU region", config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: "http://localhost:8451", Region: config.RegionEU},
			"wss://ws.eu.rhombussystems.com:8443/websocket?x-auth-scheme=api-token"},
		{"partner key with client org", config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", EndpointURL: config.DefaultEndpointURL, IsPartner: true, PartnerOrg: "org-1"},
			"wss://ws.rhombussystems.com:8443/websocket?x-auth-scheme=partner-api-token"},
		{"US oauth", freshOAuthConfig(config.DefaultEndpointURL, false, ""),
			"wss://ws.rhombussystems.com:8443/websocket?x-auth-scheme=api-oauth-token"},
		{"EU partner oauth with client org", freshOAuthConfig(config.EUEndpointURL, true, "org-1"),
			"wss://ws.eu.rhombussystems.com:8443/websocket?x-auth-scheme=partner-api-oauth-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildWSURL(wsAuthConfig(tc.cfg)); got != tc.want {
				t.Fatalf("buildWSURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWSAuthConfigUsesTokenKeyForKeyProfiles(t *testing.T) {
	cert := config.Config{AuthType: config.AuthTypeCert, ApiKey: "cert-key", WSApiKey: "ws-key", CertFile: "c", KeyFile: "k", IsPartner: true}
	h, err := buildWSHeaders(wsAuthConfig(cert))
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("x-auth-apikey") != "ws-key" || h.Get("x-auth-scheme") != "partner-api-token" {
		t.Fatalf("headers = %v, want the WS key with partner-api-token", h)
	}

	cert.WSApiKey = ""
	h, err = buildWSHeaders(wsAuthConfig(cert))
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("x-auth-apikey") != "cert-key" || h.Get("x-auth-scheme") != "partner-api-token" {
		t.Fatalf("headers = %v, want the API key with partner-api-token", h)
	}
}

func TestBuildWSHeaders(t *testing.T) {
	h, err := buildWSHeaders(wsAuthConfig(freshOAuthConfig(config.DefaultEndpointURL, true, "org-1")))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"x-auth-scheme":       "partner-api-oauth-token",
		"x-auth-access-token": "access-1",
		"x-auth-org":          "org-1",
	}
	if len(h) != len(want) {
		t.Errorf("headers = %v, want exactly %v", h, want)
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	h, err = buildWSHeaders(wsAuthConfig(config.Config{AuthType: config.AuthTypeToken, ApiKey: "k", PartnerOrg: "org-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("x-auth-apikey") != "k" || h.Get("x-auth-scheme") != "api-token" || h.Get("x-auth-org") != "org-1" {
		t.Fatalf("key headers = %v", h)
	}
	if strings.Contains(buildWSURL(config.Config{AuthType: config.AuthTypeToken, PartnerOrg: "org-1"}), "org-1") {
		t.Fatal("x-auth-org must travel as a header, not in the URL")
	}
}
