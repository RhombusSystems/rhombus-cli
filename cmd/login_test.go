package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterClientRegistersPublicClient(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/register" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"client_id":"client-1","token_endpoint_auth_method":"none"}`)
	}))
	defer srv.Close()

	id, secret, err := registerClient(srv.URL, "http://127.0.0.1:4242/callback")
	if err != nil {
		t.Fatal(err)
	}
	if id != "client-1" || secret != "" {
		t.Fatalf("client = %q / %q, want client-1 with no secret", id, secret)
	}
	if got["token_endpoint_auth_method"] != "none" {
		t.Fatalf("registration body = %v, want token_endpoint_auth_method none", got)
	}
}

func TestRegisterClientKeepsAnIssuedSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"client_id":"client-1","client_secret":"issued-secret"}`)
	}))
	defer srv.Close()

	_, secret, err := registerClient(srv.URL, "http://127.0.0.1:4242/callback")
	if err != nil || secret != "issued-secret" {
		t.Fatalf("secret = %q, err %v", secret, err)
	}
}

func tokenEndpoint(t *testing.T, check func(r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		check(r)
		fmt.Fprint(w, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExchangeCodePublicClient(t *testing.T) {
	srv := tokenEndpoint(t, func(r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("public client sent HTTP Basic auth")
		}
		if r.PostForm.Get("client_id") != "client-1" || r.PostForm.Has("client_secret") {
			t.Errorf("form = %v, want client_id only", r.PostForm)
		}
		if r.PostForm.Get("code_verifier") != "verifier" || r.PostForm.Get("grant_type") != "authorization_code" {
			t.Errorf("form = %v", r.PostForm)
		}
	})

	tok, err := exchangeCodeForToken(srv.URL, "client-1", "", "code-1", "http://127.0.0.1:4242/callback", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-1" || tok.RefreshToken != "refresh-1" || tok.ExpiresIn != 3600 {
		t.Fatalf("token = %+v", tok)
	}
}

func TestExchangeCodeConfidentialClient(t *testing.T) {
	srv := tokenEndpoint(t, func(r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "client-1" || secret != "client-secret" {
			t.Errorf("basic auth = %q/%v, want the stored client credentials", id, ok)
		}
	})

	if _, err := exchangeCodeForToken(srv.URL, "client-1", "client-secret", "code-1", "http://127.0.0.1:4242/callback", "verifier"); err != nil {
		t.Fatal(err)
	}
}

// partnerAPI answers getOrgV2 and getClientsV2 with the given statuses and
// records the scheme each call used.
func partnerAPI(t *testing.T, orgStatus, partnerStatus int) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-auth-access-token") != "access-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls = append(calls, r.URL.Path+" "+r.Header.Get("x-auth-scheme"))
		switch r.URL.Path {
		case "/api/org/getOrgV2":
			w.WriteHeader(orgStatus)
		case "/api/partner/getClientsV2":
			w.WriteHeader(partnerStatus)
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestDetectPartner(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		orgStatus, partnerStatus int
		force                    bool
		want                     bool
		wantErr                  bool
		wantCalls                []string
	}{
		{"org account", 200, 403, false, false, false,
			[]string{"/api/org/getOrgV2 api-oauth-token"}},
		{"partner account detected on 403", 403, 200, false, true, false,
			[]string{"/api/org/getOrgV2 api-oauth-token", "/api/partner/getClientsV2 partner-api-oauth-token"}},
		{"--partner skips the org check", 200, 200, true, true, false,
			[]string{"/api/partner/getClientsV2 partner-api-oauth-token"}},
		{"denied everywhere", 403, 403, false, false, true,
			[]string{"/api/org/getOrgV2 api-oauth-token", "/api/partner/getClientsV2 partner-api-oauth-token"}},
		{"server error", 500, 200, false, false, true,
			[]string{"/api/org/getOrgV2 api-oauth-token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := partnerAPI(t, tc.orgStatus, tc.partnerStatus)
			got, err := detectPartner(srv.URL, "access-1", tc.force)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("detectPartner = %v, %v; want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
			if strings.Join(*calls, ",") != strings.Join(tc.wantCalls, ",") {
				t.Fatalf("calls = %v, want %v", *calls, tc.wantCalls)
			}
		})
	}
}
