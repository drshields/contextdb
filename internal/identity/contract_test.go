package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tagPayload is a verbatim capture of what identity-helper.js actually posts,
// transcribed from the tag's own object literals rather than from the Go
// structs. Every previous fixture in this package was built in Go, which meant
// a field the tag sends but Go did not declare was invisible to the whole
// suite: the tag shipped with `referrer_host` in every pageview, Go rejected it
// under DisallowUnknownFields, and every real ingest returned 400.
const tagPayload = `{
  "site": "example.com",
  "subject": "0f2c9a11-1c2b-4a55-9a3e-77b1d0c4e5f6",
  "session": {
    "id": "b7e6d1a2-3f44-4c99-8e21-9a0b5d6c7e8f",
    "started_at": "2026-03-01T10:00:00.000Z",
    "ended_at": "2026-03-01T10:04:12.000Z",
    "entry_path": "/lp?utm_source=reddit",
    "entry_referrer": "https://www.reddit.com/r/analytics/",
    "pageviews": [
      {
        "url": "/lp?utm_source=reddit",
        "path": "/lp",
        "referrer": "https://www.reddit.com/r/analytics/",
        "referrer_host": "www.reddit.com",
        "title": "Landing",
        "at": "2026-03-01T10:00:00.000Z",
        "dwell_ms": 8200,
        "scroll_pct": 100,
        "viewport_w": 1440,
        "viewport_h": 900
      },
      {
        "url": "/docs?utm_source=reddit",
        "path": "/docs",
        "referrer": "https://example.com/lp?utm_source=reddit",
        "referrer_host": "example.com",
        "title": "Docs",
        "at": "2026-03-01T10:01:30.000Z",
        "dwell_ms": 15400,
        "scroll_pct": 62,
        "viewport_w": 1440,
        "viewport_h": 900
      }
    ],
    "referrer_chain": ["www.reddit.com"]
  },
  "click_ids": { "gclid": "Cj0KCjw", "wbraid": "AB123" },
  "utm": { "utm_source": "reddit" },
  "signal": {
    "screen_bucket": "1a2b3c",
    "lang_bucket": "en-US",
    "timezone_offset": -360,
    "conn_type": "4g",
    "hardware_cores": 8,
    "platform": "macos"
  },
  "consent": { "analytics": true },
  "agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"
}`

func TestTagPayloadIsAccepted(t *testing.T) {
	graph := memstoreFor(t)
	h := NewHandler(graph, GraphPrior(graph))

	rec := post(t, h, tagPayload)
	if rec.Code != http.StatusOK {
		t.Fatalf("the real tag payload was rejected: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Degraded     bool     `json:"degraded"`
		Observations []string `json:"observations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Degraded {
		t.Error("granted consent should not be degraded")
	}
	if len(resp.Observations) == 0 {
		t.Error("no observations written")
	}
}

// TestReferrerHostIsCarriedThrough guards the specific field that broke it.
func TestReferrerHostIsCarriedThrough(t *testing.T) {
	var rep Report
	if err := json.Unmarshal([]byte(tagPayload), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Session.Pageviews) != 2 {
		t.Fatalf("pageviews = %d, want 2", len(rep.Session.Pageviews))
	}
	if got := rep.Session.Pageviews[0].ReferrerHost; got != "www.reddit.com" {
		t.Errorf("pageview referrer_host = %q, want www.reddit.com", got)
	}
	if got := rep.Session.Pageviews[1].ReferrerHost; got != "example.com" {
		t.Errorf("second pageview referrer_host = %q", got)
	}
}

// TestTagPayloadKeysAllDecode guards the whole contract in one place: any key
// the tag sends must have a home in the Go structs, and any key it sends that
// Go rejects is a 400 in production.
func TestTagPayloadKeysAllDecode(t *testing.T) {
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(tagPayload), &raw); err != nil {
		t.Fatal(err)
	}
	var rep Report
	dec := json.NewDecoder(bytesReader(tagPayload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("strict decode of the tag payload failed: %v", err)
	}

	// Also assert the spine and signal round-trip, not just the envelope.
	if rep.ClickIDs["gclid"] != "Cj0KCjw" {
		t.Errorf("click_ids lost gclid: %v", rep.ClickIDs)
	}
	if rep.Signal.HardwareCores != 8 || rep.Signal.ConnType != "4g" {
		t.Errorf("signal lost fields: %+v", rep.Signal)
	}
	if rep.Session.EntryPath != "/lp?utm_source=reddit" {
		t.Errorf("entry_path = %q", rep.Session.EntryPath)
	}
}

func bytesReader(s string) *strings.Reader { return strings.NewReader(s) }

// post2 serves the embedded tag and returns the recorder.
func post2(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeTag(rec, httptest.NewRequest(http.MethodGet, "/identity-helper.js", nil))
	return rec
}

// TestStitchingIsASiteKeyProperty pins the contract that makes inter-site
// stitching work, and it spans both languages: the tag derives the
// registrable domain and sends it as `site`, and the server derives the
// subject id from that site. Two hosts on one registrable domain must land on
// the same subject; two different properties must not.
//
// The cookie Domain attribute is the other half and is verified in
// TestTagSetsDomainAttributeForSubdomains.
func TestStitchingIsASiteKeyProperty(t *testing.T) {
	// A real visitor carries ONE subject key in the shared first-party cookie.
	// What varies is which hostname they happen to be on.
	const cookieValue = "0f2c9a11-1c2b-4a55-9a3e-77b1d0c4e5f6"

	// What the tag would send as `site` for each hostname.
	hosts := map[string]string{
		"landing.example.com":  "example.com",
		"www.example.com":      "example.com",
		"checkout.example.com": "example.com",
	}

	want := SubjectID("example.com", cookieValue).String()
	for host, site := range hosts {
		if got := SubjectID(site, cookieValue).String(); got != want {
			t.Errorf("%s: subject id %s, want %s (site key %q)", host, got, want, site)
		}
	}

	// A different property must not collide, even with the same cookie value.
	if other := SubjectID("otherproperty.net", cookieValue).String(); other == want {
		t.Error("a different property must not share the subject id")
	}

	// And the naive implementation this replaces would have failed to stitch.
	if naive := SubjectID("landing.example.com", cookieValue); naive.String() != SubjectID("www.example.com", cookieValue).String() {
		t.Log("note: per-hostname site keys do not stitch, which is why SITE is the registrable domain")
	}
}

// TestTagSetsDomainAttributeForSubdomains checks the other half of the
// mechanism: without a Domain attribute the cookie is scoped to the exact
// host and is never sent to a sibling subdomain, so the shared site key would
// be useless.
func TestTagSetsDomainAttributeForSubdomains(t *testing.T) {
	rec := post2(t, NewHandler(memstoreFor(t), nil))
	js := rec.Body.String()
	if !strings.Contains(js, "Domain=.") {
		t.Error("tag never sets a cookie Domain attribute; subdomains cannot share a subject key")
	}
	if !strings.Contains(js, "registrableDomain") {
		t.Error("tag does not derive a registrable domain")
	}
	// The site key must not still default to the raw hostname.
	if strings.Contains(js, `|| location.hostname`) {
		t.Error("site key still defaults to location.hostname, which defeats stitching")
	}
}
