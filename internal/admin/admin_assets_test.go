package admin

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/antiartificial/contextdb/pkg/client"
)

// TestAdminIndexWithoutBuildFailsLoudly pins the contract that a binary built
// without the UI reports the missing build rather than serving a blank page.
func TestAdminIndexWithoutBuildFailsLoudly(t *testing.T) {
	_, err := resolveAdminIndex()
	if err == nil {
		t.Skip("admin UI is built; nothing to assert")
	}
	if !strings.Contains(err.Error(), "npm run admin:build") {
		t.Errorf("error should name the remedy, got: %v", err)
	}
}

var adminAssetRef = regexp.MustCompile(`(?:src|href)="/admin/(assets/[^"]+)"`)

func TestResolveAdminIndexInjectsManifestAssets(t *testing.T) {
	if _, err := adminDist.ReadFile("dist/.vite/manifest.json"); err != nil {
		t.Skip("admin UI not built; run npm run admin:build")
	}
	index, err := resolveAdminIndex()
	if err != nil {
		t.Fatalf("resolveAdminIndex: %v", err)
	}
	page := string(index)

	if strings.Contains(page, adminShellPlaceholder) {
		t.Errorf("placeholder %q was not replaced", adminShellPlaceholder)
	}

	refs := adminAssetRef.FindAllStringSubmatch(page, -1)
	if len(refs) == 0 {
		t.Fatalf("no asset references injected into shell:\n%s", page)
	}

	for _, ref := range refs {
		name := ref[1]
		if _, err := adminDist.ReadFile("dist/" + name); err != nil {
			t.Errorf("injected reference %q is not present in the embedded FS: %v", name, err)
		}
	}

	if !strings.Contains(page, `<script type="module"`) {
		t.Errorf("no module script tag injected:\n%s", page)
	}
	if !strings.Contains(page, `rel="stylesheet"`) {
		t.Errorf("no stylesheet link injected:\n%s", page)
	}
	if !strings.Contains(page, `<div id="app">`) {
		t.Errorf("shell lost its mount point:\n%s", page)
	}
}

func TestAdminIndexServesShellWithoutHashes(t *testing.T) {
	if _, err := adminDist.ReadFile("dist/.vite/manifest.json"); err != nil {
		t.Skip("admin UI not built; run npm run admin:build")
	}
	db := client.MustOpen(client.Options{Mode: client.ModeEmbedded})
	defer db.Close()

	rec := httptest.NewRecorder()
	New(db).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	page := rec.Body.String()
	// The served page must never contain a raw content hash: those are
	// build artifacts and must not be baked into committed content.
	if regexp.MustCompile(`index-[A-Za-z0-9_-]{8}\.(?:js|css)`).MatchString(page) {
		t.Errorf("served page contains a baked content hash:\n%s", page)
	}
	refs := adminAssetRef.FindAllStringSubmatch(page, -1)
	if len(refs) == 0 {
		t.Fatalf("served page has no asset references:\n%s", page)
	}

	// Every referenced asset must actually be served by the handler.
	for _, ref := range refs {
		path := "/admin/" + ref[1]
		assetRec := httptest.NewRecorder()
		New(db).ServeHTTP(assetRec, httptest.NewRequest(http.MethodGet, path, nil))
		if assetRec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, assetRec.Code)
		}
	}
}
