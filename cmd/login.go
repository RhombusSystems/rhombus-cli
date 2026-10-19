package cmd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/auth"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	loginCmd.Flags().Int("callback-port", 0, "Fixed loopback port for the OAuth redirect (default: an OS-assigned free port)")
	loginCmd.Flags().Bool("force", false, "Log in again even if the profile is already authenticated (converts an API-key or certificate profile to OAuth)")
	loginCmd.Flags().Bool("force-register", false, "Force dynamic client re-registration even if a client is already saved")
	loginCmd.Flags().Bool("partner", false, "Authenticate as a partner account. If omitted, a partner account is auto-detected when org-level access is denied.")
	loginCmd.Flags().String("region", "", "Rhombus region to log in to: us or eu (default: the profile's endpoint region)")
	rootCmd.AddCommand(loginCmd)
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with Rhombus via browser login",
	Long: "Opens a browser window for you to log into Rhombus. The profile then stores an OAuth access token and\n" +
		"refresh token (no API key is created); the CLI refreshes the access token automatically.\n\n" +
		"Profiles set up with an API key or certificate keep working. Use --force to switch one to OAuth.",
	RunE: runLogin,
}

func runLogin(cmd *cobra.Command, args []string) error {
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")
	if profile == "" {
		profile = config.DefaultProfile
	}

	cfg := config.LoadConfig(profile)

	// Nothing to do if this profile is already authenticated (API key from a prior
	// login or `rhombus configure`, or OAuth tokens). Require --force to log in again.
	force, _ := cmd.Flags().GetBool("force")
	if !force {
		if cfg.ApiKey != "" {
			fmt.Printf("Profile %q is already authenticated (API key %s).\n", profile, maskKey(cfg.ApiKey))
			fmt.Println("Nothing to do. Re-run with --force to switch this profile to browser (OAuth) login.")
			return nil
		}
		if cfg.AuthType == config.AuthTypeOAuth && cfg.OAuthRefreshToken != "" {
			fmt.Printf("Profile %q is already logged in.\n", profile)
			fmt.Println("Nothing to do. Re-run with --force to log in again.")
			return nil
		}
	}

	// Resolve the region: --region wins (and repoints the profile's endpoint);
	// otherwise use the profile's endpoint so EU customers reach the EU hosts.
	regionFlag, _ := cmd.Flags().GetString("region")
	regionFlag = strings.ToLower(strings.TrimSpace(regionFlag))
	if regionFlag != "" && regionFlag != config.RegionUS && regionFlag != config.RegionEU {
		return fmt.Errorf("invalid --region %q: use us or eu", regionFlag)
	}
	region := cfg.RegionOrDefault()
	apiEndpoint := cfg.EndpointURL
	if regionFlag != "" {
		apiEndpoint = config.EndpointForRegion(regionFlag)
	}
	regionChanged := regionFlag != "" && regionFlag != region
	if regionFlag != "" {
		region = regionFlag
	}
	authWebBaseURL := config.AuthWebBaseURLForRegion(region)
	consoleBaseURL := config.ConsoleBaseURLForRegion(region)

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
	// when we have no client, when forced, when the region changes (clients are
	// per region), or when the port differs from the one the stored client
	// registered its redirect URI with — because that redirect must carry the
	// exact port we're now listening on. New clients are public (PKCE-only, no
	// secret); a confidential client registered by an older CLI keeps its secret.
	forceRegister, _ := cmd.Flags().GetBool("force-register")
	clientID := cfg.OAuthClientID
	clientSecret := cfg.OAuthClientSecret
	needsRegister := forceRegister || clientID == "" || regionChanged || cfg.CallbackPort != port
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

	// Step 3: exchange the authorization code for access + refresh tokens.
	fmt.Println("Exchanging authorization code for token...")
	token, err := exchangeCodeForToken(authWebBaseURL, clientID, clientSecret, result.code, redirectURI, codeVerifier)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	if token.RefreshToken == "" {
		return fmt.Errorf("token exchange failed: no refresh token in response")
	}

	// Step 4: find out whether this is a partner account. A partner's token is
	// denied (HTTP 403) on org-scoped calls; it then must use the partner scheme.
	// An explicit --partner skips the org check.
	partner, _ := cmd.Flags().GetBool("partner")
	partner, err = detectPartner(apiEndpoint, token.AccessToken, partner)
	if err != nil {
		return fmt.Errorf("verifying login: %w", err)
	}

	// Step 5: store the tokens (and the region, when chosen).
	if regionFlag != "" {
		if err := config.SaveConfig(profile, "", apiEndpoint); err != nil {
			return fmt.Errorf("saving endpoint: %w", err)
		}
		if err := config.SaveRegion(profile, regionFlag); err != nil {
			return fmt.Errorf("saving region: %w", err)
		}
	}
	if err := config.SaveOAuthLogin(profile, config.OAuthTokens{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    auth.ExpiresAt(int64(token.ExpiresIn)),
	}, partner); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}

	fmt.Println()
	if partner {
		fmt.Printf("Successfully logged in as a partner account! Credentials saved to profile %q.\n", profile)
	} else {
		fmt.Printf("Successfully logged in! Credentials saved to profile %q.\n", profile)
	}
	if cfg.ApiKey != "" {
		fmt.Printf("The API key this profile used before (%s) was removed from the profile but not revoked;\n", maskKey(cfg.ApiKey))
		fmt.Println("delete it under Settings > API Management in the Rhombus console if nothing else uses it.")
	}
	fmt.Println("Run 'rhombus camera get-minimal-camera-state-list' to verify.")
	return nil
}

// detectPartner checks the new access token against the API and reports whether
// it belongs to a partner account. Unless forcePartner is set it first makes an
// org-scoped call; an HTTP 403 there means a partner account, confirmed with a
// partner-scoped call.
func detectPartner(apiEndpoint, accessToken string, forcePartner bool) (bool, error) {
	if !forcePartner {
		status, err := probeOAuth(apiEndpoint, "/api/org/getOrgV2", accessToken, false)
		if err != nil {
			return false, err
		}
		if status != http.StatusForbidden {
			return false, nil
		}
		fmt.Println("Org-level access was denied (HTTP 403); checking for a partner account...")
	}
	if _, err := probeOAuth(apiEndpoint, "/api/partner/getClientsV2", accessToken, true); err != nil {
		return false, err
	}
	return true, nil
}

// probeOAuth POSTs an empty body to path with the access token. It returns the
// status for 200 and 403 and an error for anything else.
func probeOAuth(apiEndpoint, path, accessToken string, partner bool) (int, error) {
	req, err := http.NewRequest("POST", apiEndpoint+path, strings.NewReader("{}"))
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(auth.HeaderScheme, auth.Scheme(config.Config{AuthType: config.AuthTypeOAuth, IsPartner: partner}))
	req.Header.Set(auth.HeaderAccessToken, accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK || (resp.StatusCode == http.StatusForbidden && !partner) {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, string(body))
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
// an OAuth application manually. The CLI registers as a public client
// (token_endpoint_auth_method "none", RFC 8252): PKCE protects the code exchange
// and no client secret is stored. It returns the issued client_id and the
// client_secret, which is empty unless the server issued one anyway; persisting
// them (so later logins reuse the same client) is the caller's responsibility.
func registerClient(authWebBaseURL, redirectURI string) (clientID, clientSecret string, err error) {
	// Register the exact loopback redirect URI (including port) that this login will
	// use — the Rhombus auth server matches it exactly at authorize/token time.
	reqBody := map[string]any{
		"client_name":                "Rhombus CLI",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
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
	if reg.ClientID == "" {
		return "", "", fmt.Errorf("registration response missing client_id: %s", string(body))
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
// A public client (no secret) sends only its client_id in the form body. A
// confidential client first authenticates via client_secret_basic (HTTP Basic,
// the common default), and if the server rejects that auth method it retries
// with client_secret_post (credentials in the form body).
func exchangeCodeForToken(authWebBaseURL, clientID, clientSecret, code, redirectURI, codeVerifier string) (*tokenResponse, error) {
	if clientSecret == "" {
		token, err := requestToken(authWebBaseURL, clientID, "", code, redirectURI, codeVerifier, false)
		if err != nil && strings.Contains(err.Error(), "invalid_client") {
			err = fmt.Errorf("%w (re-run with --force-register to register a new client)", err)
		}
		return token, err
	}
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
		// client_secret_post: client_id + secret travel in the request body; a
		// public client sends only client_id. (With Basic auth these MUST NOT also
		// appear in the body — RFC 6749 §2.3 forbids using more than one client
		// authentication method per request.)
		form.Set("client_id", clientID)
		if clientSecret != "" {
			form.Set("client_secret", clientSecret)
		}
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
