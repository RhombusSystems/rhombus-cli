package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Generated commands end to end through the root command: a partner-only command runs as the partner organization
// (the selected client org is not sent), an org API command still sends it, and a multipart upload posts its
// fields and file.
func TestGeneratedCommandsHonorPartnerOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, env := range []string{"RHOMBUS_API_KEY", "RHOMBUS_PROFILE", "RHOMBUS_OUTPUT", "RHOMBUS_ENDPOINT_URL", "CLAUDECODE"} {
		t.Setenv(env, "")
	}

	type seen struct {
		authOrg     string
		contentType string
		fields      map[string]string
		fileName    string
	}
	var mu sync.Mutex
	requests := map[string]seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := seen{authOrg: r.Header.Get("x-auth-org"), contentType: r.Header.Get("Content-Type"),
			fields: map[string]string{}}
		if reader, err := r.MultipartReader(); err == nil {
			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				if part.FileName() != "" {
					s.fileName = part.FormName() + ":" + part.FileName()
				} else {
					s.fields[part.FormName()] = string(data)
				}
			}
		}
		mu.Lock()
		requests[r.URL.Path] = s
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	const clientOrg = "AAAAAAAAAAAAAAAAAAAAAA"
	pdf := filepath.Join(t.TempDir(), "PO-17.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := []string{"--api-key", "k", "--endpoint-url", srv.URL, "--partner-org", clientOrg}

	run := func(args ...string) {
		t.Helper()
		rootCmd.SetArgs(append(args, common...))
		rootCmd.SetOut(io.Discard)
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rhombus %v: %v", args, err)
		}
	}

	run("partner", "quote-pool-order", "--request-id", "r1", "--product-code", "SKU", "--quantity", "1")
	run("partner-portal", "get-quote-products")
	run("partner-portal", "upload-p-o-to-opp", "--field", "opportunityId=006Ab", "--field", "poNumber=PO-17",
		"--field", "portalFileId=f-1", "--file", pdf)
	run("camera", "get-minimal-camera-state-list")

	for _, path := range []string{"/api/partner/license/quotePoolOrder", "/api/partnerportal/getQuoteProducts",
		"/api/partnerportal/uploadPOToOpp"} {
		s, ok := requests[path]
		if !ok {
			t.Fatalf("%s was not called", path)
		}
		if s.authOrg != "" {
			t.Errorf("%s sent x-auth-org %q; partner-only commands must not", path, s.authOrg)
		}
	}
	if got := requests["/api/camera/getMinimalCameraStateList"].authOrg; got != clientOrg {
		t.Errorf("camera command x-auth-org = %q, want %q", got, clientOrg)
	}

	upload := requests["/api/partnerportal/uploadPOToOpp"]
	if upload.fileName != "file:PO-17.pdf" || upload.fields["opportunityId"] != "006Ab" ||
		upload.fields["poNumber"] != "PO-17" || upload.fields["portalFileId"] != "f-1" {
		t.Errorf("upload = %+v (Content-Type %q)", upload, upload.contentType)
	}
}
