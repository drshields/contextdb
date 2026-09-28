package identity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/antiartificial/contextdb/internal/core"
)

// TestSecondVisitIsWeightedAsRepeat is the end-to-end claim: ingesting the same
// click ID twice must produce a higher-weight edge the second time, without the
// caller doing anything special.
func TestSecondVisitIsWeightedAsRepeat(t *testing.T) {
	graph := memstoreFor(t)
	h := NewHandler(graph, GraphPrior(graph))
	body, _ := json.Marshal(sampleReport(t))

	first := post(t, h, string(body))
	if first.Code != http.StatusOK {
		t.Fatalf("first visit: %d %s", first.Code, first.Body.String())
	}
	second := post(t, h, string(body))
	if second.Code != http.StatusOK {
		t.Fatalf("second visit: %d %s", second.Code, second.Body.String())
	}

	subjectID := SubjectID("example.com", "subject-abc")
	edges, err := graph.EdgesFrom(t.Context(), "example.com", subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}

	// After two ingests the click-id observation should exist at the repeat
	// weight, because the prior now reports the first sighting.
	var maxWeight float64
	var kinds []string
	for _, e := range edges {
		k, _ := e.Properties["kind"].(string)
		kinds = append(kinds, k)
		if k == KindClickID && e.Weight > maxWeight {
			maxWeight = e.Weight
		}
	}
	if maxWeight != WeightClickIDRepeat {
		t.Errorf("click id weight after repeat visit = %v, want %v (kinds seen: %v)",
			maxWeight, WeightClickIDRepeat, kinds)
	}
	if !strings.Contains(second.Body.String(), `"ok":true`) {
		t.Errorf("second response: %s", second.Body.String())
	}
}

// TestWithheldConsentStillBuildsLinkage proves degradation is not suppression.
func TestWithheldConsentStillBuildsLinkage(t *testing.T) {
	graph := memstoreFor(t)
	h := NewHandler(graph, GraphPrior(graph))
	rep := sampleReport(t)
	rep.Consent.Analytics = false
	body, _ := json.Marshal(rep)

	rec := post(t, h, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Degraded     bool     `json:"degraded"`
		Observations []string `json:"observations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Degraded {
		t.Error("withheld consent must be reported as degraded, not silently dropped")
	}
	if len(resp.Observations) == 0 {
		t.Error("withheld consent must not discard first-party session behaviour")
	}
}

// TestUnknownFieldRejected guards the boundary: the client is untrusted, so
// the decoder refuses to silently accept fields it does not understand.
func TestUnknownFieldRejected(t *testing.T) {
	graph := memstoreFor(t)
	h := NewHandler(graph, nil)
	rec := post(t, h, `{"site":"e.com","subject":"s","session":{"id":"x"},"totally_new_field":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unrecognised field", rec.Code)
	}
}

// TestMarkRoundTripsIntoEvidence is the regression test for a dead API. An
// earlier draft exposed ctxdbIdentity.mark() and accepted a mark field, but
// Resolve never read it: the handler parked it in the source record's
// properties, where it was invisible to evidence queries and overwritten on
// every subsequent mark. A 78-test suite missed it because the tests checked
// the tag's served shape, never a mark actually arriving in the graph.
func TestMarkRoundTripsIntoEvidence(t *testing.T) {
	graph := memstoreFor(t)
	h := NewHandler(graph, GraphPrior(graph))

	markReport := func() Report {
		rep := sampleReport(t)
		rep.Mark = &Mark{
			Kind:   "crm_identity",
			Detail: map[string]any{"account_id": "acct-88", "tier": "enterprise"},
		}
		return rep
	}
	body, _ := json.Marshal(markReport())

	first := post(t, h, string(body))
	if first.Code != http.StatusOK {
		t.Fatalf("mark ingest: %d %s", first.Code, first.Body.String())
	}

	subjectID := SubjectID("example.com", "subject-abc")
	edges, err := graph.EdgesFrom(t.Context(), "example.com", subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}

	var found *core.Edge
	for i := range edges {
		if edges[i].Properties["kind"] == KindMark {
			found = &edges[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("mark produced no evidence edge; kinds present: %v", edgeKinds(edges))
	}
	if found.Weight != WeightMark {
		t.Errorf("first mark weight = %v, want %v", found.Weight, WeightMark)
	}
	if found.ValidUntil != nil {
		t.Error("a CRM confirmation must not expire with the session that carried it")
	}

	obs, err := graph.GetNode(t.Context(), "example.com", found.Dst)
	if err != nil {
		t.Fatalf("mark observation not in graph: %v", err)
	}
	detail, _ := obs.Properties["detail"].(map[string]any)
	if detail["kind"] != "crm_identity" {
		t.Errorf("mark kind = %v, want crm_identity", detail["kind"])
	}
	inner, _ := detail["detail"].(map[string]any)
	if inner["account_id"] != "acct-88" {
		t.Errorf("mark detail lost: %v", detail["detail"])
	}

	// A repeat promotes once, to a ceiling, and no further. The original 0.5
	// edge is deliberately still there: the same observation was *held* at
	// 0.5 and later gained support, and an append-only system should not
	// erase that it was once less sure. Consumers read the strongest active
	// relates_to edge per observation.
	second := post(t, h, string(body))
	if second.Code != http.StatusOK {
		t.Fatalf("repeat mark ingest: %d %s", second.Code, second.Body.String())
	}
	edges, _ = graph.EdgesFrom(t.Context(), "example.com", subjectID, nil)

	var promoted bool
	var sawOriginal bool
	for i := range edges {
		if edges[i].Properties["kind"] != KindMark {
			continue
		}
		switch edges[i].Weight {
		case WeightMarkCorroborated:
			promoted = true
		case WeightMark:
			sawOriginal = true
		default:
			t.Errorf("mark weight %v is neither base nor corroborated", edges[i].Weight)
		}
		if edges[i].Weight >= WeightClickIDRepeat {
			t.Errorf("a browser-supplied mark must never outrank a platform-minted click id: %v >= %v",
				edges[i].Weight, WeightClickIDRepeat)
		}
	}
	if !promoted {
		t.Error("repeat mark should promote to the corroborated weight")
	}
	if !sawOriginal {
		t.Error("the original lower-confidence edge should survive; erasing it would discard the history of how sure we were")
	}

	// And it must not climb further on a third sighting.
	third := post(t, h, string(body))
	if third.Code != http.StatusOK {
		t.Fatalf("third mark ingest: %d %s", third.Code, third.Body.String())
	}
	edges, _ = graph.EdgesFrom(t.Context(), "example.com", subjectID, nil)
	for i := range edges {
		if edges[i].Properties["kind"] == KindMark && edges[i].Weight != WeightMark && edges[i].Weight != WeightMarkCorroborated {
			t.Errorf("third sighting climbed to %v; promotion must be capped", edges[i].Weight)
		}
	}
}

func edgeKinds(edges []core.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if k, ok := e.Properties["kind"].(string); ok {
			out = append(out, k)
		}
	}
	return out
}

func TestAbsentMarkEmitsNothing(t *testing.T) {
	r := Resolve(sampleReport(t), &Prior{SeenMarks: map[string]int{}})
	if n := len(edgeByKind(r, KindMark)); n != 0 {
		t.Errorf("a report with no mark should emit no mark edge, got %d", n)
	}
}

func TestMarkWithEmptyKindIsIgnored(t *testing.T) {
	rep := sampleReport(t)
	rep.Mark = &Mark{Kind: ""}
	r := Resolve(rep, &Prior{SeenMarks: map[string]int{}})
	if n := len(edgeByKind(r, KindMark)); n != 0 {
		t.Errorf("an empty mark kind should be ignored, got %d edges", n)
	}
}

func TestMarkWeightSitsBelowClickIDAndAboveSignal(t *testing.T) {
	if WeightMark <= WeightCoarseSignal {
		t.Errorf("a host mark (%v) should outrank a coarse signal (%v)", WeightMark, WeightCoarseSignal)
	}
	if WeightMarkCorroborated >= WeightClickIDRepeat {
		t.Errorf("corroborated mark (%v) must stay below click id repeat (%v)",
			WeightMarkCorroborated, WeightClickIDRepeat)
	}
}
