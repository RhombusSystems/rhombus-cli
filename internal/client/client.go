package client

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/RhombusSystems/rhombus-cli/internal/auth"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/RhombusSystems/rhombus-cli/internal/params"
)

var (
	httpClients   = make(map[string]*http.Client)
	httpClientsMu sync.Mutex
)

func APICall(cfg config.Config, path string, body map[string]any) (map[string]any, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	return post(cfg, path, "application/json", jsonBody, string(jsonBody))
}

// MultipartCall POSTs a multipart/form-data body: the form fields in order, then each file as a part with its base
// name as the filename and Content-Type application/octet-stream, which the API reads as undeclared and types from
// the file extension.
func MultipartCall(cfg config.Config, path string, fields []params.FormField, files []params.FilePart) (map[string]any, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	summary := make([]string, 0, len(fields)+len(files))

	for _, field := range fields {
		if err := writer.WriteField(field.Name, field.Value); err != nil {
			return nil, fmt.Errorf("writing form field %s: %w", field.Name, err)
		}
		summary = append(summary, fmt.Sprintf("%s=%s", field.Name, field.Value))
	}

	for _, file := range files {
		data, err := os.ReadFile(file.Path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file.Path, err)
		}
		part, err := writer.CreateFormFile(file.Name, filepath.Base(file.Path))
		if err != nil {
			return nil, fmt.Errorf("writing file part %s: %w", file.Name, err)
		}
		if _, err := part.Write(data); err != nil {
			return nil, fmt.Errorf("writing file part %s: %w", file.Name, err)
		}
		summary = append(summary, fmt.Sprintf("%s=@%s (%d bytes)", file.Name, filepath.Base(file.Path), len(data)))
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("closing multipart body: %w", err)
	}

	// The verbose log lists field values and file names, never file contents.
	return post(cfg, path, writer.FormDataContentType(), buf.Bytes(), strings.Join(summary, "\n> "))
}

// post sends body to path with the profile's credential headers and returns the parsed JSON response. An OAuth
// token the server rejects (HTTP 401) is refreshed and the request retried once with the same body.
func post(cfg config.Config, path, contentType string, body []byte, verboseBody string) (map[string]any, error) {
	client := http.DefaultClient
	if auth.UsesClientCert(cfg) {
		// mTLS cert-based auth
		var err error
		client, err = getCertClient(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}
	}

	url := cfg.EndpointURL + path

	var resp *http.Response
	var respBody []byte
	for attempt := 0; ; attempt++ {
		authHeaders, err := auth.Headers(cfg)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "application/json")
		for k, v := range authHeaders {
			req.Header[k] = v
		}

		if cfg.Verbose {
			fmt.Fprintf(os.Stderr, "> %s %s\n", req.Method, req.URL)
			for k, vals := range req.Header {
				for _, v := range vals {
					if k == "X-Auth-Apikey" || k == "X-Auth-Access-Token" {
						if len(v) > 4 {
							v = "****" + v[len(v)-4:]
						}
					}
					fmt.Fprintf(os.Stderr, "> %s: %s\n", k, v)
				}
			}
			fmt.Fprintf(os.Stderr, ">\n> %s\n>\n", verboseBody)
		}

		resp, err = client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request failed: %w", err)
		}
		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			retry, err := auth.Retry401(cfg, authHeaders)
			if err != nil {
				return nil, err
			}
			if retry {
				if cfg.Verbose {
					fmt.Fprintf(os.Stderr, "< HTTP %s (refreshed OAuth token, retrying)\n", resp.Status)
				}
				continue
			}
		}
		break
	}

	if cfg.Verbose {
		fmt.Fprintf(os.Stderr, "< HTTP %s\n", resp.Status)
		for k, vals := range resp.Header {
			for _, v := range vals {
				fmt.Fprintf(os.Stderr, "< %s: %s\n", k, v)
			}
		}
		fmt.Fprintf(os.Stderr, "<\n< %s\n<\n", string(respBody))
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

// GetHTTPClient returns the appropriate HTTP client for the given config (mTLS or default).
func GetHTTPClient(cfg config.Config) (*http.Client, error) {
	if auth.UsesClientCert(cfg) {
		return getCertClient(cfg.CertFile, cfg.KeyFile)
	}
	return http.DefaultClient, nil
}

// GetMediaHTTPClient returns an HTTP client for media server requests.
// The media server uses a self-signed cert, so TLS verification is relaxed.
// Uses mTLS client cert when available. Every request carries the profile's
// credential headers (see auth.Transport); callers add only request-specific
// headers such as a LAN federated-token cookie.
func GetMediaHTTPClient(cfg config.Config) (*http.Client, error) {
	if auth.UsesClientCert(cfg) {
		certPEM, err := os.ReadFile(cfg.CertFile)
		if err != nil {
			return nil, fmt.Errorf("reading cert file: %w", err)
		}
		keyPEM, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("reading key file: %w", err)
		}

		certBlock, _ := pem.Decode(certPEM)
		if certBlock == nil {
			return nil, fmt.Errorf("failed to decode certificate PEM")
		}
		keyBlock, _ := pem.Decode(keyPEM)
		if keyBlock == nil {
			return nil, fmt.Errorf("failed to decode key PEM")
		}

		cert := tls.Certificate{
			Certificate: [][]byte{certBlock.Bytes},
		}
		cert.PrivateKey, err = parsePrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing private key: %w", err)
		}

		return &http.Client{
			Transport: &auth.Transport{Cfg: cfg, Base: &http.Transport{
				TLSClientConfig: &tls.Config{
					Certificates:       []tls.Certificate{cert},
					InsecureSkipVerify: true,
				},
			}},
		}, nil
	}

	return &http.Client{
		Transport: &auth.Transport{Cfg: cfg, Base: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}},
	}, nil
}

// getCertClient returns a cached HTTP client configured with the given client certificate.
// Uses raw PEM loading to handle certificates with negative serial numbers
// (generated by the Rhombus CA using Java's BigInteger from random UUIDs).
func getCertClient(certFile, keyFile string) (*http.Client, error) {
	cacheKey := certFile + "|" + keyFile

	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()

	if c, ok := httpClients[cacheKey]; ok {
		return c, nil
	}

	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("reading cert file: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading key file: %w", err)
	}

	// Parse the PEM blocks directly without x509 serial number validation
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("failed to decode certificate PEM")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("failed to decode key PEM")
	}

	cert := tls.Certificate{
		Certificate: [][]byte{certBlock.Bytes},
	}

	cert.PrivateKey, err = parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
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

func parsePrivateKey(der []byte) (any, error) {
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("unsupported private key type")
}
