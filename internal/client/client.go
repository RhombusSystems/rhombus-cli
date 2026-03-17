package client

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

var (
	httpClients   = make(map[string]*http.Client)
	httpClientsMu sync.Mutex
)

func APICall(cfg config.Config, path string, body map[string]any) (map[string]any, error) {
	if cfg.ApiKey == "" {
		return nil, fmt.Errorf("no API key configured. Run 'rhombus login' or 'rhombus configure'")
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	url := cfg.EndpointURL + path
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-auth-apikey", cfg.ApiKey)

	var client *http.Client

	if cfg.AuthType == config.AuthTypeCert && cfg.CertFile != "" && cfg.KeyFile != "" {
		// mTLS cert-based auth
		req.Header.Set("x-auth-scheme", "api")
		client, err = getCertClient(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}
	} else {
		// Token-based auth
		req.Header.Set("x-auth-scheme", "api-token")
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("failed to parse response JSON: %w", err)
		}
	}

	return result, nil
}

// getCertClient returns a cached HTTP client configured with the given client certificate.
func getCertClient(certFile, keyFile string) (*http.Client, error) {
	cacheKey := certFile + "|" + keyFile

	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()

	if c, ok := httpClients[cacheKey]; ok {
		return c, nil
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading cert/key pair: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	httpClients[cacheKey] = client
	return client, nil
}
