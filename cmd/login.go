package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	loginCmd.Flags().Int("callback-port", 0, "Fixed loopback port for the OAuth redirect (default: an OS-assigned free port)")
	loginCmd.Flags().Bool("force", false, "Re-authenticate even if the profile already has an API key")
	loginCmd.Flags().Bool("force-register", false, "Force dynamic client re-registration even if a client is already saved")
	loginCmd.Flags().Bool("partner", false, "Authenticate as a partner account (mint a partner-level API key). If omitted, a partner account is auto-detected when org-level minting is denied.")
	rootCmd.AddCommand(loginCmd)
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with Rhombus via browser login",
	Long: "Opens a browser window for you to log into Rhombus. Once authenticated, your CLI credentials are configured automatically.\n\n" +
		"Your logged-in user must have permission to create an API key.",
	RunE: runLogin,
}

func runLogin(cmd *cobra.Command, args []string) error {
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")
	if profile == "" {
		profile = config.DefaultProfile
	}

	cfg := config.LoadConfig(profile)

	// If this profile already has an API key (from a prior login or `rhombus
	// configure`), there's nothing to do — logging in would just mint a
	// redundant key. Require --force to re-authenticate.
	force, _ := cmd.Flags().GetBool("force")
	if cfg.ApiKey != "" && !force {
		fmt.Printf("Profile %q is already authenticated (API key %s).\n", profile, maskKey(cfg.ApiKey))
		fmt.Println("Nothing to do. Re-run with --force to authenticate again and mint a new API key.")
		return nil
	}

	// Resolve region from the profile's configured endpoint so EU customers are
	// sent to the EU auth/console hosts.
	region := config.RegionForEndpoint(cfg.EndpointURL)
	authWebBaseURL := config.AuthWebBaseURLForRegion(region)
	consoleBaseURL := config.ConsoleBaseURLForRegion(region)
	apiEndpoint := cfg.EndpointURL

	// Step 1: bind the loopback listener. The Rhombus auth server matches the
	// registered redirect URI exactly (including port), so we must know the port
	// before registering. The port is dynamic but sticky: an explicit --callback-port
	// wins; otherwise we prefer the port a prior registration used (so we can reuse
	// that client), falling back to an OS-assigned free port if it's taken or unset.
	flagPort, _ := cmd.Flags().GetInt("callback-port")
	requestedPort := flagPort
	if requestedPort == 0 {
		requestedPort = cfg.CallbackPort // 0 the first time
	}

	callbackResult := make(chan callbackData, 1)
	listener, port, err := startCallbackServer(callbackResult, requestedPort)
	if err != nil {
		if flagPort != 0 {
			return fmt.Errorf("starting callback server: %w", err)
		}
		// Preferred port was unavailable and none was explicitly requested — let the
		// OS assign a free one (this changes the port and triggers re-registration).
		listener, port, err = startCallbackServer(callbackResult, 0)
		if err != nil {
			return fmt.Errorf("starting callback server: %w", err)
		}
	}
	defer listener.Close()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// Step 2: obtain an OAuth client via dynamic client registration. We register
	// when we have no client, when forced, or when the port differs from the one the
	// stored client registered its redirect URI with — because that redirect must
	// carry the exact port we're now listening on.
	forceRegister, _ := cmd.Flags().GetBool("force-register")
	clientID := cfg.OAuthClientID
	clientSecret := cfg.OAuthClientSecret
	needsRegister := forceRegister || clientID == "" || clientSecret == "" || cfg.CallbackPort != port
	if needsRegister {
		fmt.Println("Registering OAuth client with Rhombus...")
		id, secret, err := registerClient(authWebBaseURL, redirectURI)
		if err != nil {
			return fmt.Errorf("dynamic client registration failed: %w", err)
		}
		clientID, clientSecret = id, secret
		if err := config.SaveOAuthClient(profile, clientID, clientSecret, port); err != nil {
			return fmt.Errorf("saving registered client: %w", err)
		}
	}

	// PKCE + CSRF state.
	codeVerifier, err := generateCodeVerifier()
	if err != nil {
		return fmt.Errorf("generating PKCE verifier: %w", err)
	}
	codeChallenge := generateCodeChallenge(codeVerifier)
	state, err := generateRandomString(32)
	if err != nil {
		return fmt.Errorf("generating state: %w", err)
	}

	authURL := buildAuthorizeURL(consoleBaseURL, clientID, redirectURI, state, codeChallenge)

	fmt.Println("Opening browser to log in to Rhombus...")
	fmt.Printf("Listening for the OAuth callback on %s\n", redirectURI)
	fmt.Println()
	fmt.Printf("If the browser doesn't open, visit this URL:\n%s\n\n", authURL)

	openBrowser(authURL)
	fmt.Println("Waiting for authentication...")

	var result callbackData
	select {
	case result = <-callbackResult:
		listener.Close()
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("authentication timed out after 5 minutes")
	}

	if result.err != nil {
		return fmt.Errorf("authentication failed: %w", result.err)
	}
	if result.state != state {
		return fmt.Errorf("authentication failed: state mismatch (possible CSRF attack)")
	}
	if result.code == "" {
		return fmt.Errorf("authentication failed: no authorization code received")
	}

	// Step 3: exchange the authorization code for an access token.
	fmt.Println("Exchanging authorization code for token...")
	token, err := exchangeCodeForToken(authWebBaseURL, clientID, clientSecret, result.code, redirectURI, codeVerifier)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}

	// Step 4: use the OAuth access token to mint a long-lived API key. A
	// cert-based key (mTLS) is preferred; fall back to a token-based key if the
	// server cannot issue a cert.
	//
	// Partner accounts must mint through the partner endpoint. The org endpoint
	// authenticates the same OAuth token, but the resulting org-level principal
	// lacks org API-administration permission, so the server's RBAC layer denies
	// it with a (bare) HTTP 403. An explicit --partner forces the partner path;
	// otherwise we try the org path first and, on a 403, retry as a partner.
	partner, _ := cmd.Flags().GetBool("partner")

	fmt.Println("Minting API key...")
	_, err = createApiKey(apiEndpoint, token.AccessToken, profile, partner, true)
	if err != nil {
		_, err = createApiKey(apiEndpoint, token.AccessToken, profile, partner, false)
	}
	if err != nil && !partner && isForbidden(err) {
		fmt.Println("Org-level API key was denied (HTTP 403); retrying as a partner account...")
		partner = true
		_, err = createApiKey(apiEndpoint, token.AccessToken, profile, partner, true)
		if err != nil {
			_, err = createApiKey(apiEndpoint, token.AccessToken, profile, partner, false)
		}
	}
	if err != nil {
		return fmt.Errorf("failed to create API key: %w", err)
	}

	// Also create a token-based key for services that don't support cert auth (e.g. WebSocket).
	if tokenKey, tokenErr := createTokenOnlyApiKey(apiEndpoint, token.AccessToken, profile, partner); tokenErr == nil && tokenKey != "" {
		config.SaveField(profile, "ws_api_key", tokenKey)
	}

	fmt.Println()
	fmt.Printf("Successfully logged in! Credentials saved to profile %q.\n", profile)
	fmt.Println("Run 'rhombus camera get-minimal-camera-state-list' to verify.")
	return nil
}

// isForbidden reports whether err originated from an HTTP 403 response. The
// API-key mint helpers wrap the status as "HTTP <code>: <body>", so a partner
// account denied at the org endpoint surfaces here as an HTTP 403.
func isForbidden(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 403")
}

type callbackData struct {
	code  string
	state string
	err   error
}

// tokenResponse models a standard OAuth 2.0 token endpoint response
// (RFC 6749 §5.1) as well as its error response (§5.2).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// registerClient performs OAuth 2.0 Dynamic Client Registration (RFC 7591) against
// the auth server's /oauth/register endpoint, so the user does not have to register
// an OAuth application manually. It returns the newly issued client_id/client_secret;
// persisting them (so later logins reuse the same client) is the caller's responsibility.
func registerClient(authWebBaseURL, redirectURI string) (clientID, clientSecret string, err error) {
	// Register the exact loopback redirect URI (including port) that this login will
	// use — the Rhombus auth server matches it exactly at authorize/token time.
	reqBody := map[string]any{
		"client_name":   "Rhombus CLI",
		"redirect_uris": []string{redirectURI},
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", authWebBaseURL+"/oauth/register", strings.NewReader(string(jsonBody)))
	if err != nil {
		return "", "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &reg); err != nil {
		return "", "", fmt.Errorf("parsing registration response: %w", err)
	}
	if reg.ClientID == "" || reg.ClientSecret == "" {
		return "", "", fmt.Errorf("registration response missing client credentials: %s", string(body))
	}
	return reg.ClientID, reg.ClientSecret, nil
}

func buildAuthorizeURL(consoleBaseURL, clientID, redirectURI, state, codeChallenge string) string {
	params := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return fmt.Sprintf("%s/oauth/authorize?%s", consoleBaseURL, params.Encode())
}

// startCallbackServer binds a loopback listener for the OAuth redirect. A port of
// 0 lets the OS pick a free ephemeral port; the port actually bound is returned.
func startCallbackServer(result chan<- callbackData, port int) (net.Listener, int, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, 0, fmt.Errorf("cannot listen on 127.0.0.1:%d for the OAuth callback (is it already in use? pass a different --callback-port): %w", port, err)
	}
	boundPort := listener.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		state := q.Get("state")
		errMsg := q.Get("error")

		if errMsg != "" {
			errDesc := q.Get("error_description")
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, resultPage("Authentication Failed", "Error: "+errMsg+" — "+errDesc+". You can close this tab."))
			result <- callbackData{err: fmt.Errorf("%s: %s", errMsg, errDesc)}
			return
		}

		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, resultPage("Authentication Successful", "You can close this tab and return to your terminal."))
		result <- callbackData{code: code, state: state}
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()

	return listener, boundPort, nil
}

// exchangeCodeForToken performs the standard OAuth 2.0 authorization-code + PKCE
// token exchange. The Rhombus auth server's /oauth/token endpoint expects a
// form-encoded request (application/x-www-form-urlencoded), not JSON.
//
// It first authenticates the client via client_secret_basic (HTTP Basic, the
// common default), and if the server rejects that auth method it retries with
// client_secret_post (credentials in the form body).
func exchangeCodeForToken(authWebBaseURL, clientID, clientSecret, code, redirectURI, codeVerifier string) (*tokenResponse, error) {
	token, err := requestToken(authWebBaseURL, clientID, clientSecret, code, redirectURI, codeVerifier, true)
	if err != nil && strings.Contains(err.Error(), "invalid_client") {
		token, err = requestToken(authWebBaseURL, clientID, clientSecret, code, redirectURI, codeVerifier, false)
	}
	return token, err
}

func requestToken(authWebBaseURL, clientID, clientSecret, code, redirectURI, codeVerifier string, useBasicAuth bool) (*tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {codeVerifier},
	}
	if !useBasicAuth {
		// client_secret_post: client_id + secret travel in the request body.
		// (With Basic auth these MUST NOT also appear in the body — RFC 6749 §2.3
		// forbids using more than one client authentication method per request.)
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequest("POST", authWebBaseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if useBasicAuth {
		// client_secret_basic: credentials travel in the Authorization header.
		req.SetBasicAuth(clientID, clientSecret)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	// Parse best-effort so we can surface a structured OAuth error if present.
	var token tokenResponse
	if len(body) > 0 {
		_ = json.Unmarshal(body, &token)
	}

	if resp.StatusCode != http.StatusOK {
		if token.Error != "" {
			return nil, fmt.Errorf("HTTP %d: %s: %s", resp.StatusCode, token.Error, token.ErrorDescription)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	if token.AccessToken == "" {
		return nil, fmt.Errorf("no access token in response: %s", string(body))
	}

	return &token, nil
}

// createApiKey uses the OAuth access token to mint a long-lived API key. If
// partner=true, it uses the partner endpoint. If useCert=true, it generates a
// CSR for cert-based (mTLS) auth and stores the returned signed cert; otherwise
// it creates a token-based key.
func createApiKey(endpointURL, oauthAccessToken, profile string, partner, useCert bool) (string, error) {
	endpoint := "/api/integrations/org/submitApiTokenApplication"
	if partner {
		endpoint = "/api/partner/submitApiTokenApplication"
	}

	reqBody := map[string]string{
		"displayName": "Rhombus CLI",
	}

	var privateKey *ecdsa.PrivateKey

	if useCert {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", fmt.Errorf("generating private key: %w", err)
		}
		privateKey = key

		csrTemplate := x509.CertificateRequest{
			Subject: pkix.Name{CommonName: "rhombus-cli"},
		}
		csrDER, err := x509.CreateCertificateRequest(rand.Reader, &csrTemplate, privateKey)
		if err != nil {
			return "", fmt.Errorf("creating CSR: %w", err)
		}
		csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

		reqBody["authType"] = "CERT"
		reqBody["csr"] = string(csrPEM)
	} else {
		reqBody["authType"] = "API_TOKEN"
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", endpointURL+endpoint, strings.NewReader(string(jsonBody)))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if partner {
		req.Header.Set("x-auth-scheme", "partner-api-oauth-token")
	} else {
		req.Header.Set("x-auth-scheme", "api-oauth-token")
	}
	req.Header.Set("x-auth-access-token", oauthAccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ApiKey   string `json:"apiKey"`
		Cert     string `json:"cert"`
		ValidCSR bool   `json:"validCSR"`
		Error    bool   `json:"error"`
		ErrorMsg string `json:"errorMsg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}

	if result.Error {
		return "", fmt.Errorf("%s", result.ErrorMsg)
	}
	if result.ApiKey == "" {
		return "", fmt.Errorf("no API key returned")
	}

	// Save credentials
	if useCert && result.Cert != "" && privateKey != nil {
		certDir := config.ProfileCertDir(profile)
		if err := os.MkdirAll(certDir, 0700); err != nil {
			return "", fmt.Errorf("creating cert dir: %w", err)
		}

		certFile := certDir + "/client.crt"
		keyFile := certDir + "/client.key"

		keyDER, err := x509.MarshalECPrivateKey(privateKey)
		if err != nil {
			return "", fmt.Errorf("marshaling private key: %w", err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

		if err := os.WriteFile(certFile, []byte(result.Cert), 0600); err != nil {
			return "", fmt.Errorf("writing cert: %w", err)
		}
		if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
			return "", fmt.Errorf("writing key: %w", err)
		}

		if err := config.SaveCertCredentials(profile, result.ApiKey, certFile, keyFile, partner); err != nil {
			return "", fmt.Errorf("saving credentials: %w", err)
		}
	} else {
		if err := config.SaveTokenCredentials(profile, result.ApiKey, partner); err != nil {
			return "", fmt.Errorf("saving credentials: %w", err)
		}
	}

	return result.ApiKey, nil
}

// createTokenOnlyApiKey creates a token-based API key for services that don't support cert auth.
func createTokenOnlyApiKey(endpointURL, oauthAccessToken, profile string, partner bool) (string, error) {
	endpoint := "/api/integrations/org/submitApiTokenApplication"
	if partner {
		endpoint = "/api/partner/submitApiTokenApplication"
	}

	reqBody := map[string]string{
		"displayName": "Rhombus CLI (WebSocket)",
		"authType":    "API_TOKEN",
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", endpointURL+endpoint, strings.NewReader(string(jsonBody)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if partner {
		req.Header.Set("x-auth-scheme", "partner-api-oauth-token")
	} else {
		req.Header.Set("x-auth-scheme", "api-oauth-token")
	}
	req.Header.Set("x-auth-access-token", oauthAccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ApiKey   string `json:"apiKey"`
		Error    bool   `json:"error"`
		ErrorMsg string `json:"errorMsg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.Error {
		return "", fmt.Errorf("%s", result.ErrorMsg)
	}

	return result.ApiKey, nil
}

// PKCE helpers

func generateCodeVerifier() (string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateRandomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

// Browser helpers

func openBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "linux":
		cmd = exec.Command("xdg-open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		return
	}
	_ = cmd.Start()
}

func resultPage(title, message string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<title>%s — Rhombus CLI</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Nunito+Sans:wght@400;700;900&display=swap" rel="stylesheet">
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: 'Nunito Sans', -apple-system, BlinkMacSystemFont, sans-serif;
    display: flex;
    width: 100%%;
    height: 100vh;
  }
  .left {
    flex-grow: 1;
    background: linear-gradient(120deg, #00536A, #17323B);
    display: flex;
    align-items: center;
    justify-content: center;
  }
  .left svg { width: 120px; height: 120px; opacity: 0.15; }
  .right {
    width: 50%%;
    max-width: 600px;
    background: #fff;
    display: flex;
    flex-direction: column;
    justify-content: center;
    align-items: center;
    text-align: center;
    padding: 2.5rem;
  }
  .logo {
    width: 140px;
    margin-bottom: 24px;
  }
  h1 {
    color: #0B0C0D;
    font-size: 28px;
    font-weight: 900;
    line-height: normal;
    margin-bottom: 12px;
  }
  p {
    color: #55585C;
    font-size: 15px;
    line-height: 1.5;
  }
  .check {
    width: 56px;
    height: 56px;
    border-radius: 50%%;
    background: #2A7DE1;
    display: flex;
    align-items: center;
    justify-content: center;
    margin: 0 auto 20px;
  }
  .check svg { width: 28px; height: 28px; }
  .footer {
    color: #AEB3B8;
    font-size: 12px;
    margin-top: 40px;
  }
  @media (max-width: 900px) {
    .left { display: none; }
    .right { width: 100%%; max-width: none; }
  }
</style>
</head>
<body>
  <div class="left">
    <svg viewBox="0 0 100 100" fill="white"><rect x="20" y="20" width="60" height="60" rx="8" transform="rotate(45 50 50)"/></svg>
  </div>
  <div class="right">
    <svg class="logo" viewBox="0 0 600 120" xmlns="http://www.w3.org/2000/svg">
      <defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1"><stop offset="0%%" stop-color="#46cce2"/><stop offset="100%%" stop-color="#0077b6"/></linearGradient></defs>
      <g transform="translate(10,10)"><rect width="70" height="70" rx="10" transform="rotate(45 35 35)" fill="url(#g)"/><circle cx="35" cy="35" r="10" fill="white"/></g>
      <text x="120" y="82" font-family="'Nunito Sans',sans-serif" font-size="72" font-weight="900" fill="#1a2332">rhombus</text>
    </svg>
    <div class="check">
      <svg fill="none" viewBox="0 0 24 24" stroke="white" stroke-width="3"><path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7"/></svg>
    </div>
    <h1>%s</h1>
    <p>%s</p>
    <div class="footer">&copy; Rhombus, Inc %d</div>
  </div>
</body>
</html>`, title, title, message, time.Now().Year())
}
