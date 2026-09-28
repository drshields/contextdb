package identity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
