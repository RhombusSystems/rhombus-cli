package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/RhombusSystems/rhombus-cli/internal/params"
)

func TestMultipartCallSendsFieldsAndFile(t *testing.T) {
	pdf := []byte("%PDF-1.7\nfake purchase order\n")
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "PO-17.pdf")
	if err := os.WriteFile(pdfPath, pdf, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/partnerportal/uploadPOToOpp" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-auth-apikey"); got != "key-123" {
			t.Errorf("x-auth-apikey = %q", got)
		}
		if got := r.Header.Get("x-auth-scheme"); got != "partner-api-token" {
			t.Errorf("x-auth-scheme = %q", got)
		}
		if got := r.Header.Get("x-auth-org"); got != "" {
			t.Errorf("x-auth-org = %q, want none", got)
		}

		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("not multipart: %v (Content-Type %q)", err, r.Header.Get("Content-Type"))
		}
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(part)
			if part.FileName() == "" {
				fields[part.FormName()] = string(data)
				continue
			}
			if part.FormName() != "file" || part.FileName() != "PO-17.pdf" {
				t.Errorf("file part name=%q filename=%q", part.FormName(), part.FileName())
			}
			if ct := part.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("file part Content-Type = %q, want application/octet-stream", ct)
			}
			if string(data) != string(pdf) {
				t.Errorf("file content = %q", data)
			}
		}
		want := map[string]string{"opportunityId": "006Ab", "poNumber": "PO-17", "portalFileId": "f-1"}
		for k, v := range want {
			if fields[k] != v {
				t.Errorf("field %s = %q, want %q", k, fields[k], v)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"portalFileId": "f-1", "error": false})
	}))
	defer srv.Close()

	cfg := config.Config{EndpointURL: srv.URL, ApiKey: "key-123", AuthType: "token", IsPartner: true}
	result, err := MultipartCall(cfg, "/api/partnerportal/uploadPOToOpp",
		[]params.FormField{{Name: "opportunityId", Value: "006Ab"}, {Name: "poNumber", Value: "PO-17"},
			{Name: "portalFileId", Value: "f-1"}},
		[]params.FilePart{{Name: "file", Path: pdfPath}})
	if err != nil {
		t.Fatal(err)
	}
	if result["portalFileId"] != "f-1" {
		t.Errorf("result = %v", result)
	}
}

func TestMultipartCallMissingFile(t *testing.T) {
	cfg := config.Config{EndpointURL: "http://127.0.0.1:1", ApiKey: "k", AuthType: "token"}
	_, err := MultipartCall(cfg, "/api/x", nil, []params.FilePart{{Name: "file", Path: "/does/not/exist.pdf"}})
	if err == nil {
		t.Fatal("want an error for a missing file")
	}
}

// APICall still sends JSON after the refactor onto the shared post helper.
func TestAPICallSendsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["deviceUuid"] != "d1" {
			t.Errorf("body = %v (%v)", body, err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	result, err := APICall(config.Config{EndpointURL: srv.URL, ApiKey: "k", AuthType: "token"}, "/api/camera/x",
		map[string]any{"deviceUuid": "d1"})
	if err != nil || result["ok"] != true {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestAPICallReportsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":true,"errorCode":"AUTH_ORG_NOT_ALLOWED"}`))
	}))
	defer srv.Close()

	_, err := APICall(config.Config{EndpointURL: srv.URL, ApiKey: "k", AuthType: "token"}, "/api/x", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") ||
		!strings.Contains(err.Error(), "AUTH_ORG_NOT_ALLOWED") {
		t.Fatalf("err = %v", err)
	}
}
