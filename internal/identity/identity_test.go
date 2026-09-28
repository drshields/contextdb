package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/antiartificial/contextdb/internal/core"
	memstore "github.com/antiartificial/contextdb/internal/store/memory"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return ts
}

func sampleReport(t *testing.T) Report {
	return Report{
		Site:    "example.com",
		Subject: "subject-abc",
		Session: Session{
			ID:        "sess-1",
			StartedAt: mustTime(t, "2026-03-01T10:00:00Z"),
			EndedAt:   mustTime(t, "2026-03-01T10:04:00Z"),
			EntryPath: "/landing",
			EntryRef:  "https://news.ycombinator.com/item?id=1",
			Pageviews: []Pageview{
				{Path: "/landing", At: mustTime(t, "2026-03-01T10:00:00Z")},
				{Path: "/docs", At: mustTime(t, "2026-03-01T10:01:00Z")},
				{Path: "/pricing", At: mustTime(t, "2026-03-01T10:02:00Z")},
			},
			ReferrerChain: []string{"news.ycombinator.com", "example.com"},
		},
		ClickIDs: map[string]string{"gclid": "abc123", "wbraid": "wbraid-9"},
		UTM:      map[string]string{"utm_source": "reddit"},
		Signal:   Signal{ScreenBucket: "1a2b3c", LangBucket: "en-US", TimezoneOff: -360},
		Consent:  Consent{Analytics: true},
		Agent:    "Mozilla/5.0 (test)",
	}
}

func kindsOf(r Resolved) map[string]int {
	out := map[string]int{}
	for _, e := range r.Edges {
		if k, ok := e.Properties["kind"].(string); ok {
			out[k]++
		}
	}
	return out
}

func edgeByKind(r Resolved, kind string) []core.Edge {
	var out []core.Edge
	for _, e := range r.Edges {
		if k, ok := e.Properties["kind"].(string); ok && k == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestResolveEmitsEveryObservationKind(t *testing.T) {
	got := kindsOf(Resolve(sampleReport(t), &Prior{}))
	for _, kind := range []string{KindSession, KindClickID, KindUTM, KindReferrer, KindSignal} {
		if got[kind] == 0 {
			t.Errorf("no %s edge emitted; got %v", kind, got)
		}
	}
}

func TestSelfReferrerIsNotIdentityEvidence(t *testing.T) {
	r := Resolve(sampleReport(t), &Prior{})
	for _, e := range edgeByKind(r, KindReferrer) {
		if _, ok := e.Properties["kind"].(string); !ok {
			t.Fatal("edge missing kind")
		}
		for _, o := range r.Observations {
			if o.ID != e.Dst {
				continue
			}
			detail, _ := o.Properties["detail"].(map[string]any)
			if host, _ := detail["host"].(string); host == "example.com" {
				t.Errorf("self-referrer %q must not be emitted as external evidence", host)
			}
		}
	}
	if n := len(edgeByKind(r, KindReferrer)); n != 1 {
		t.Errorf("expected 1 external referrer edge, got %d", n)
	}
}

func TestClickIDRepeatOutweighsFirstSighting(t *testing.T) {
	first := edgeByKind(Resolve(sampleReport(t), &Prior{}), KindClickID)
	prior := &Prior{SeenClickIDs: map[string]bool{"gclid:abc123": true, "wbraid:wbraid-9": true}}
	second := edgeByKind(Resolve(sampleReport(t), prior), KindClickID)

	if len(first) == 0 || len(second) == 0 {
		t.Fatal("expected click id edges both times")
	}
	if second[0].Weight <= first[0].Weight {
		t.Errorf("repeat sighting should outweigh first: %v vs %v", second[0].Weight, first[0].Weight)
	}
	if first[0].Weight != WeightClickIDSeen {
		t.Errorf("first sighting weight = %v, want %v", first[0].Weight, WeightClickIDSeen)
	}
}

func TestCoarseSignalExpiresWithSession(t *testing.T) {
	r := Resolve(sampleReport(t), &Prior{})
	edges := edgeByKind(r, KindSignal)
	if len(edges) != 1 {
		t.Fatalf("expected 1 signal edge, got %d", len(edges))
	}
	e := edges[0]
	if e.ValidUntil == nil {
		t.Fatal("coarse signal must not be open-ended; it would accumulate into a persistent identifier")
	}
	if !e.ValidUntil.After(e.ValidFrom) {
		t.Errorf("signal expiry %v must be after %v", e.ValidUntil, e.ValidFrom)
	}
	if e.Weight != WeightCoarseSignal {
		t.Errorf("signal weight = %v, want %v", e.Weight, WeightCoarseSignal)
	}
	// Everything else must be durable: the session spine and click IDs are
	// the evidence worth keeping.
	for _, kind := range []string{KindSession, KindClickID} {
		for _, d := range edgeByKind(r, kind) {
			if d.ValidUntil != nil {
				t.Errorf("%s edge should be durable, got expiry %v", kind, d.ValidUntil)
			}
		}
	}
}

func TestUTMIsNeverIdentityEvidence(t *testing.T) {
	r := Resolve(sampleReport(t), &Prior{})
	for _, e := range edgeByKind(r, KindUTM) {
		if e.Weight > 0.25 {
			t.Errorf("utm weight %v is too high to be safe as identity evidence", e.Weight)
		}
	}
}

func TestWithheldConsentDegradesButStillIngests(t *testing.T) {
	rep := sampleReport(t)
	rep.Consent.Analytics = false
	r := Resolve(rep, &Prior{})
	if !r.Degraded {
		t.Error("withheld consent should mark the record degraded")
	}
	if len(r.Observations) == 0 {
		t.Error("withheld consent should not silently discard the session spine")
	}
	if r.Subject.Properties["degraded"] != true {
		t.Errorf("subject should record degradation, got %v", r.Subject.Properties["degraded"])
	}
}

func TestResolutionIsPure(t *testing.T) {
	rep := sampleReport(t)
	a := Resolve(rep, &Prior{})
	b := Resolve(rep, &Prior{})
	if a.Subject.ID != b.Subject.ID {
		t.Error("Resolve is not pure: subject id changed between identical calls")
	}
	if len(a.Observations) != len(b.Observations) {
		t.Error("Resolve is not pure: observation count changed")
	}
}

func TestSubjectIDStableAcrossReingest(t *testing.T) {
	first := SubjectID("https://www.Example.com/", "abc")
	second := SubjectID("example.com", "abc")
	if first != second {
		t.Errorf("site normalization broken: %v vs %v", first, second)
	}
	if SubjectID("other.com", "abc") == first {
		t.Error("two sites must not share a subject id")
	}
	if SubjectID("example.com", "xyz") == first {
		t.Error("two subjects must not share an id")
	}
}

func TestObservationIDScopedToSubject(t *testing.T) {
	detail := map[string]any{"path": "/pricing"}
	a := ObservationID("example.com", "subject-a", KindPageview, detail)
	b := ObservationID("example.com", "subject-b", KindPageview, detail)
	if a == b {
		t.Error("two visitors observing the same page must not collide into one node")
	}
	if a != ObservationID("example.com", "subject-a", KindPageview, detail) {
		t.Error("same observation must hash to the same node for idempotency")
	}
}

func TestValidateRejectsIncompleteReports(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Report)
	}{
		{"no site", func(r *Report) { r.Site = "" }},
		{"no subject", func(r *Report) { r.Subject = "" }},
		{"no session", func(r *Report) { r.Session.ID = "" }},
		{"inverted times", func(r *Report) {
			r.Session.StartedAt = mustTime(t, "2026-03-01T11:00:00Z")
			r.Session.EndedAt = mustTime(t, "2026-03-01T10:00:00Z")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := sampleReport(t)
			tc.mutate(&rep)
			if err := validate(&rep); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestValidateBackfillsEntryPathFromSpine(t *testing.T) {
	rep := sampleReport(t)
	rep.Session.EntryPath = ""
	if err := validate(&rep); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.Session.EntryPath != "/landing" {
		t.Errorf("entry path = %q, want /landing from the spine", rep.Session.EntryPath)
	}
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/identity/ingest", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Ingest(rec, req)
	return rec
}

func TestIngestWritesGraph(t *testing.T) {
	graph := memstore.NewGraphStore()
	h := NewHandler(graph, nil)

	raw, err := json.Marshal(sampleReport(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, string(raw))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		OK           bool     `json:"ok"`
		Subject      string   `json:"subject"`
		Observations []string `json:"observations"`
		Edges        int      `json:"edges"`
		Degraded     bool     `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.Observations) == 0 || resp.Edges == 0 {
		t.Errorf("unexpected response: %+v", resp)
	}
	if resp.Degraded {
		t.Error("granted consent should not be reported as degraded")
	}

	ctx := context.Background()
	subjectID, err := uuid.Parse(resp.Subject)
	if err != nil {
		t.Fatalf("subject id %q: %v", resp.Subject, err)
	}
	node, err := graph.GetNode(ctx, "example.com", subjectID)
	if err != nil {
		t.Fatalf("subject not in graph: %v", err)
	}
	if !node.HasLabel(LabelSubject) {
		t.Errorf("subject missing label, got %v", node.Labels)
	}
	if node.Properties["subject_key"] != "subject-abc" {
		t.Errorf("subject_key = %v", node.Properties["subject_key"])
	}
	if node.Properties["consent"] != true {
		t.Errorf("consent should be recorded on the subject, got %v", node.Properties["consent"])
	}

	// Observations must be addressable and carry their weight.
	var found bool
	for _, o := range resp.Observations {
		oid, err := uuid.Parse(o)
		if err != nil {
			t.Fatalf("observation id %q: %v", o, err)
		}
		obs, err := graph.GetNode(ctx, "example.com", oid)
		if err != nil {
			t.Fatalf("observation %s not in graph: %v", o, err)
		}
		if obs.HasLabel(LabelObservation) {
			found = true
		}
	}
	if !found {
		t.Error("no observation node carried the observation label")
	}
}

func TestIngestRejectsMalformedAndUnknownFields(t *testing.T) {
	h := NewHandler(memstore.NewGraphStore(), nil)

	if rec := post(t, h, `{"site":`); rec.Code != http.StatusBadRequest {
		t.Errorf("truncated body: status = %d, want 400", rec.Code)
	}
	if rec := post(t, h, `{"site":"a.com","subject":"s","session":{"id":"x"},"surprise":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: status = %d, want 400", rec.Code)
	}
	if rec := post(t, h, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty report: status = %d, want 400", rec.Code)
	}
}

func TestIngestRejectsWrongMethod(t *testing.T) {
	h := NewHandler(memstore.NewGraphStore(), nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/identity/ingest", nil)
	rec := httptest.NewRecorder()
	h.Ingest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestPriorDrivesWeightingEndToEnd(t *testing.T) {
	graph := memstore.NewGraphStore()
	var seenPrior *Prior
	h := NewHandler(graph, func(_ context.Context, _, _ string) (*Prior, error) {
		seenPrior = &Prior{SeenClickIDs: map[string]bool{"gclid:abc123": true}}
		return seenPrior, nil
	})

	raw, _ := json.Marshal(sampleReport(t))
	rec := post(t, h, string(raw))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if seenPrior == nil || !seenPrior.SeenClickIDs["gclid:abc123"] {
		t.Fatal("prior lookup was not consulted")
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestHelperIsServedAndSelfContained(t *testing.T) {
	h := NewHandler(memstore.NewGraphStore(), nil)
	rec := httptest.NewRecorder()
	h.ServeTag(rec, httptest.NewRequest(http.MethodGet, "/identity-helper.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, needle := range []string{"sendBeacon", "click_ids", "referrer_chain", "visibilitychange"} {
		if !strings.Contains(body, needle) {
			t.Errorf("served tag missing %q", needle)
		}
	}
	// The tag must not reintroduce the techniques it documents avoiding.
	// Comments are stripped first, otherwise the prose describing the
	// avoidance trips the very check meant to enforce it.
	code := stripJSComments(body)
	for _, banned := range []string{"canvas", "AudioContext", "fonts.check", "getFingerprints", "indexedDB"} {
		if strings.Contains(code, banned) {
			t.Errorf("served tag references fingerprinting primitive %q in executable code", banned)
		}
	}
}

// stripJSComments removes // and /* */ comments so assertions about the tag's
// behaviour are not satisfied or violated by its documentation.
func stripJSComments(src string) string {
	var out strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			i += 2
			for i < len(src) && !strings.HasPrefix(src[i:], "*/") {
				i++
			}
			i += 2
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return out.String()
}

// seededGraph returns a graph with one full report already ingested, plus the
// handler that produced it.
func seededGraph(t *testing.T) (*memstore.GraphStore, *Handler) {
	t.Helper()
	graph := memstore.NewGraphStore()
	h := NewHandler(graph, nil)
	raw, err := json.Marshal(sampleReport(t))
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(t, h, string(raw)); rec.Code != http.StatusOK {
		t.Fatalf("seed ingest: %d %s", rec.Code, rec.Body.String())
	}
	return graph, h
}

// memstoreFor is a small constructor alias so the e2e tests read cleanly.
func memstoreFor(t *testing.T) *memstore.GraphStore {
	t.Helper()
	return memstore.NewGraphStore()
}
