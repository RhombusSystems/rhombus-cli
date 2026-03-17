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

const (
	oauthClientID     = "PJjjlcKAQPCzIcaeprzEVg"
	oauthClientSecret = "kixFP1l8c55dDt0WdeX8BNwUlnFknGTr9qdn3AYKpsM"
	authBaseURL       = "https://auth.rhombussystems.com"
	consoleBaseURL    = "https://console.rhombussystems.com"
	callbackPort      = 11434
	callbackRedirect  = "http://localhost:11434/callback"
)

func init() {
	rootCmd.AddCommand(loginCmd)
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with Rhombus via browser login",
	Long:  "Opens a browser window for you to log into Rhombus. Once authenticated, your CLI credentials are configured automatically.",
	RunE:  runLogin,
}

func runLogin(cmd *cobra.Command, args []string) error {
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")
	if profile == "" {
		profile = config.DefaultProfile
	}

	// Generate PKCE code verifier and challenge
	codeVerifier, err := generateCodeVerifier()
	if err != nil {
		return fmt.Errorf("generating PKCE verifier: %w", err)
	}
	codeChallenge := generateCodeChallenge(codeVerifier)

	// Generate state parameter for CSRF protection
	state, err := generateRandomString(32)
	if err != nil {
		return fmt.Errorf("generating state: %w", err)
	}

	// Start local HTTP server to receive the callback
	callbackResult := make(chan callbackData, 1)
	listener, err := startCallbackServer(callbackResult)
	if err != nil {
		return fmt.Errorf("starting callback server: %w", err)
	}
	defer listener.Close()

	redirectURI := callbackRedirect

	// Build authorization URL
	authURL := buildAuthorizeURL(redirectURI, state, codeChallenge)

	fmt.Println("Opening browser to log in to Rhombus...")
	fmt.Println()
	fmt.Printf("If the browser doesn't open, visit this URL:\n%s\n\n", authURL)

	openBrowser(authURL)

	fmt.Println("Waiting for authentication...")

	// Wait for callback or timeout
	select {
	case result := <-callbackResult:
		listener.Close()
		if result.err != nil {
			return fmt.Errorf("authentication failed: %w", result.err)
		}
		if result.state != state {
			return fmt.Errorf("authentication failed: state mismatch (possible CSRF attack)")
		}

		var oauthAccessToken string

		if result.accessToken != "" {
			oauthAccessToken = result.accessToken
		} else if result.code != "" {
			fmt.Println("Exchanging authorization code for token...")
			token, err := exchangeCodeForToken(result.code, redirectURI, codeVerifier)
			if err != nil {
				return fmt.Errorf("token exchange failed: %w", err)
			}
			oauthAccessToken = token.AccessToken
		} else {
			return fmt.Errorf("authentication failed: no token or code received")
		}

		// Use the OAuth token to create a permanent API key
		cfg := config.LoadConfig(profile)
		isPartner := result.isPartner

		// Try cert-based auth first, fall back to token-based
		_, err = createApiKey(cfg.EndpointURL, oauthAccessToken, profile, isPartner, true)
		if err != nil {
			_, err = createApiKey(cfg.EndpointURL, oauthAccessToken, profile, isPartner, false)
		}
		if err != nil {
			return fmt.Errorf("failed to create API key: %w", err)
		}

		if isPartner {
			fmt.Println()
			fmt.Println("Partner account detected!")
			fmt.Printf("Credentials saved to profile \"%s\".\n", profile)
			fmt.Println()
			fmt.Println("To make API calls as a client org, use the --partner-org flag:")
			fmt.Println("  rhombus --partner-org <client-org-uuid> camera get-minimal-camera-state-list")
			fmt.Println()
			fmt.Println("To list your client orgs:")
			fmt.Println("  rhombus partner get-partner-clients")
		} else {
			fmt.Println()
			fmt.Printf("Successfully logged in! Credentials saved to profile \"%s\".\n", profile)
			fmt.Println("Run 'rhombus camera get-minimal-camera-state-list' to verify.")
		}
		return nil

	case <-time.After(5 * time.Minute):
		return fmt.Errorf("authentication timed out after 5 minutes")
	}
}

type callbackData struct {
	code        string
	accessToken string
	state       string
	isPartner   bool
	err         error
}

type tokenResponse struct {
	AccessToken              string `json:"accessToken"`
	RefreshToken             string `json:"refreshToken"`
	AccessTokenExpirationSec int    `json:"accessTokenExpirationSec"`
	Error                    bool   `json:"error"`
	ErrorMsg                 string `json:"errorMsg"`
}

func buildAuthorizeURL(redirectURI, state, codeChallenge string) string {
	params := url.Values{
		"type":      {"oauth"},
		"client_id": {oauthClientID},
		"redirect":  {redirectURI},
		"state":     {state},
		"challenge": {codeChallenge},
	}
	return fmt.Sprintf("%s/login?%s", consoleBaseURL, params.Encode())
}

func startCallbackServer(result chan<- callbackData) (net.Listener, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", callbackPort))
	if err != nil {
		return nil, fmt.Errorf("port %d already in use: %w", callbackPort, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		accessToken := q.Get("accessToken")
		state := q.Get("state")
		isPartner := q.Get("isPartner") == "true"
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
		result <- callbackData{code: code, accessToken: accessToken, state: state, isPartner: isPartner}
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()

	return listener, nil
}

func exchangeCodeForToken(code, redirectURI, codeVerifier string) (*tokenResponse, error) {
	reqBody := map[string]string{
		"grantType":         "AUTHORIZATION_CODE",
		"authorizationCode": code,
		"clientId":          oauthClientID,
		"clientSecret":      oauthClientSecret,
		"redirectUri":       redirectURI,
		"codeVerifier":      codeVerifier,
		"codeChallengeType": "S256",
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", authBaseURL+"/oauth/token", strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-auth-scheme", "web2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	if token.Error {
		return nil, fmt.Errorf("%s", token.ErrorMsg)
	}

	return &token, nil
}

// createApiKey attempts to create an API key. If partner=true, uses the partner endpoint.
// If useCert=true, generates a CSR for cert-based auth; otherwise creates a token.
func createApiKey(endpointURL, oauthAccessToken, profile string, partner, useCert bool) (string, error) {
	endpoint := "/api/integrations/org/submitApiTokenApplication"
	if partner {
		endpoint = "/partner/submitApiTokenApplication"
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
<head><title>%s — Rhombus CLI</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; background: #f5f5f5; }
  .card { background: white; border-radius: 12px; padding: 48px; text-align: center; box-shadow: 0 2px 8px rgba(0,0,0,0.1); max-width: 400px; }
  h1 { color: #1a1a1a; font-size: 24px; margin-bottom: 12px; }
  p { color: #666; font-size: 16px; }
</style></head>
<body><div class="card"><h1>%s</h1><p>%s</p></div></body>
</html>`, title, title, message)
}
