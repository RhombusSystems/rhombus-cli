package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testSpec = `{
  "paths": {
    "/api/partner/getClients": {"post": {"operationId": "getClients", "tags": ["Partner Webservice"],
      "parameters": [{"name": "x-auth-scheme", "in": "header", "example": "partner-api-token"},
                     {"name": "x-auth-org", "in": "header"}],
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Req"}}}}}},
    "/api/partnerportal/getQuote": {"post": {"operationId": "getQuote", "tags": ["Partner Portal Webservice"],
      "parameters": [{"name": "x-auth-scheme", "in": "header", "schema": {"default": "api-token"}}],
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Req"}}}}}},
    "/api/partnerportal/uploadPOToOpp": {"post": {"operationId": "uploadPOToOpp", "tags": ["Partner Portal Webservice"],
      "parameters": [{"name": "x-auth-scheme", "in": "header", "example": "partner-api-token"}],
      "requestBody": {"content": {"multipart/form-data": {"schema": {"$ref": "#/components/schemas/FormDataMultiPart"}}}}}},
    "/api/camera/getCameras": {"post": {"operationId": "getCameras", "tags": ["Camera Webservice"],
      "parameters": [{"name": "x-auth-scheme", "in": "header", "example": "api-token"},
                     {"name": "X-Auth-Org", "in": "header"}],
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Req"}}}}}},
    "/api/upload/floorplan": {"post": {"operationId": "uploadFloorplan", "tags": ["Upload Webservice"],
      "parameters": [{"name": "x-auth-scheme", "in": "header", "example": "api-token"},
                     {"name": "x-auth-org", "in": "header"}],
      "requestBody": {"content": {"multipart/form-data": {"schema": {"$ref": "#/components/schemas/FormDataMultiPart"}}}}}},
    "/api/legacy/getThing": {"post": {"operationId": "getThing", "tags": ["Legacy Webservice"],
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Req"}}}}}}
  },
  "components": {"schemas": {
    "Req": {"type": "object", "properties": {"orgUuid": {"type": "string"}}},
    "FormDataMultiPart": {"type": "object"}
  }}
}`

func parseOps(t *testing.T, specPath string) map[string]Operation {
	t.Helper()
	services, err := ParseSpec(specPath)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	ops := map[string]Operation{}
	for _, svc := range services {
		for _, op := range svc.Operations {
			ops[op.Path] = op
		}
	}
	return ops
}

func TestPartnerOnlyAndMultipartFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(testSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := parseOps(t, path)

	cases := []struct {
		path        string
		partnerOnly bool
		multipart   bool
		why         string
	}{
		{"/api/partner/getClients", true, false, "partner scheme example, even though x-auth-org is still listed"},
		{"/api/partnerportal/getQuote", true, false, "declares x-auth-scheme but no x-auth-org"},
		{"/api/partnerportal/uploadPOToOpp", true, true, "partner scheme and a multipart body"},
		{"/api/camera/getCameras", false, false, "org API endpoint (header name case-insensitive)"},
		{"/api/upload/floorplan", false, true, "org API multipart upload"},
		{"/api/legacy/getThing", false, false, "no auth parameters at all: not partner-only"},
	}
	for _, c := range cases {
		op, ok := ops[c.path]
		if !ok {
			t.Fatalf("%s not parsed", c.path)
		}
		if op.PartnerOnly != c.partnerOnly {
			t.Errorf("%s PartnerOnly = %v, want %v (%s)", c.path, op.PartnerOnly, c.partnerOnly, c.why)
		}
		if op.Multipart != c.multipart {
			t.Errorf("%s Multipart = %v, want %v (%s)", c.path, op.Multipart, c.multipart, c.why)
		}
	}
}

// The checked-in spec: the order-placing endpoints are partner-only, the PO upload is multipart, and org API
// endpoints keep sending the selected client org.
func TestCheckedInSpecMarksPartnerOrderEndpoints(t *testing.T) {
	ops := parseOps(t, "../openapi.json")

	for _, path := range []string{
		"/api/partner/license/getCatalog",
		"/api/partner/license/quotePoolOrder",
		"/api/partner/license/acceptPoolOrder",
		"/api/partner/getClients",
		"/api/partnerportal/getQuoteProducts",
		"/api/partnerportal/createQuote",
		"/api/partnerportal/updateQuote",
		"/api/partnerportal/uploadPOToOpp",
	} {
		op, ok := ops[path]
		if !ok {
			t.Fatalf("%s missing from openapi.json", path)
		}
		if !op.PartnerOnly {
			t.Errorf("%s should be partner-only", path)
		}
	}

	if !ops["/api/partnerportal/uploadPOToOpp"].Multipart || !ops["/api/partnerportal/uploadMdfClaimFile"].Multipart {
		t.Error("partner portal uploads should be multipart")
	}
	if op := ops["/api/camera/getMinimalCameraStateList"]; op.PartnerOnly || op.Multipart {
		t.Errorf("getMinimalCameraStateList: PartnerOnly=%v Multipart=%v, want false/false", op.PartnerOnly, op.Multipart)
	}
}
