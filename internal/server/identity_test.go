package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antiartificial/contextdb/pkg/client"
)

func identityServer(t *testing.T) http.Handler {
	t.Helper()
	db := client.MustOpen(client.Options{Mode: client.ModeEmbedded})
	t.Cleanup(func() { db.Close() })
	return NewRESTServer(db).Handler()
}

func TestIdentityHelperIsServedFromTheBinary(t *testing.T) {
	h := identityServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/identity-helper.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("served tag is empty")
	}
}

func TestIdentityIngestIsMounted(t *testing.T) {
	h := identityServer(t)
	body := `{"site":"example.com","subject":"s1","session":{"id":"sess","started_at":"2026-03-01T10:00:00Z","ended_at":"2026-03-01T10:01:00Z","entry_path":"/"}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/identity/ingest", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Errorf("response = %v", resp)
	}
	if resp["subject"] == nil || resp["subject"] == "" {
		t.Error("ingest should return the subject id so a caller can debug linkage")
	}
}

func TestIdentityIngestRejectsGarbage(t *testing.T) {
	h := identityServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/identity/ingest", strings.NewReader("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestIdentityIngestHandlesPreflight(t *testing.T) {
	h := identityServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/v1/identity/ingest", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("preflight must allow the origin; the tag is loaded cross-site by design")
	}
}
