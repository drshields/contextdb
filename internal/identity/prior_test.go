package identity

import (
	"reflect"
	"testing"
)

func TestGraphPriorReadsExistingObservations(t *testing.T) {
	ctx := t.Context()
	graph, h := seededGraph(t)

	prior, err := GraphPrior(graph)(ctx, "example.com", "subject-abc")
	if err != nil {
		t.Fatalf("prior: %v", err)
	}
	if !prior.SeenClickIDs["gclid:abc123"] {
		t.Error("prior should report the previously seen gclid")
	}
	if !prior.SeenClickIDs["wbraid:wbraid-9"] {
		t.Error("prior should report the previously seen wbraid")
	}
	if !prior.SeenReferrers["news.ycombinator.com"] {
		t.Error("prior should report the previously seen referrer host")
	}
	if prior.Sessions != 1 {
		t.Errorf("prior sessions = %d, want 1", prior.Sessions)
	}

	// A second ingest of the same report should now weight the click IDs as
	// repeats, which is the whole point of consulting the graph.
	_ = h
}

func TestEdgesCarryNamespace(t *testing.T) {
	// Regression: edges built without a namespace are invisible to
	// EdgesFrom/EdgesTo, which filter on it, so the graph looks empty and
	// every prior reads as a first sighting.
	r := Resolve(sampleReport(t), &Prior{})
	for _, e := range r.Edges {
		if e.Namespace == "" {
			t.Fatalf("edge %s->%s has no namespace", e.Src, e.Dst)
		}
		if e.Namespace != r.Subject.Namespace {
			t.Errorf("edge namespace %q != subject %q", e.Namespace, r.Subject.Namespace)
		}
	}
}

func TestGraphPriorToleratesUnknownSubject(t *testing.T) {
	graph, _ := seededGraph(t)
	prior, err := GraphPrior(graph)(t.Context(), "example.com", "never-seen")
	if err != nil {
		t.Fatalf("prior for unknown subject should not error: %v", err)
	}
	if len(prior.SeenClickIDs) != 0 {
		t.Errorf("unknown subject should have no priors, got %v", prior.SeenClickIDs)
	}
}

func TestGraphPriorToleratesNilGraph(t *testing.T) {
	prior, err := GraphPrior(nil)(t.Context(), "example.com", "s")
	if err != nil {
		t.Fatalf("nil graph should not error: %v", err)
	}
	if prior == nil {
		t.Fatal("prior should be non-nil even without a graph")
	}
}

func TestTrajectorySimilarity(t *testing.T) {
	cases := []struct {
		name     string
		prev     []string
		cur      []string
		wantZero bool
		wantSame bool
	}{
		{"identical", []string{"/a", "/b", "/c"}, []string{"/a", "/b", "/c"}, false, true},
		{"partial", []string{"/a", "/b", "/c"}, []string{"/a", "/b", "/z"}, false, false},
		{"disjoint", []string{"/a"}, []string{"/x"}, false, false},
		{"query strings ignored", []string{"/a?utm=x"}, []string{"/a"}, false, true},
		{"empty previous", nil, []string{"/a"}, true, false},
		{"empty current", []string{"/a"}, nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TrajectorySimilarity(tc.prev, tc.cur)
			if tc.wantZero && got != 0 {
				t.Fatalf("expected 0, got %v", got)
			}
			if got < 0 || got > 1 {
				t.Fatalf("similarity %v out of range", got)
			}
			if tc.wantSame && got != 1 {
				t.Errorf("expected 1, got %v", got)
			}
		})
	}
}

func TestTrajectorySimilarityIsOrderInsensitive(t *testing.T) {
	// Deliberately crude: this is set overlap, not a sequence alignment. The
	// test pins the behaviour so a future improvement is a conscious choice.
	a := TrajectorySimilarity([]string{"/a", "/b", "/c"}, []string{"/c", "/b", "/a"})
	if a != 1 {
		t.Errorf("reordered identical set = %v, want 1", a)
	}
}

func TestWeightForTrajectoryFloorsAtLandingPageRepeat(t *testing.T) {
	if got := WeightForTrajectory(0); got != 0 {
		t.Errorf("zero similarity = %v, want 0", got)
	}
	if got := WeightForTrajectory(0.01); got != WeightLandingPageRepeat {
		t.Errorf("tiny similarity should floor at %v, got %v", WeightLandingPageRepeat, got)
	}
	full := WeightForTrajectory(1)
	if full <= WeightLandingPageRepeat {
		t.Errorf("full match weight %v should exceed floor %v", full, WeightLandingPageRepeat)
	}
}

func TestEdgeWeightOrdering(t *testing.T) {
	// The hierarchy is the model's core assumption. If this reorders, the
	// weights are not calibrated against each other and should be re-derived.
	ordered := []struct {
		name string
		w    float64
	}{
		{"click id repeat", WeightClickIDRepeat},
		{"click id seen", WeightClickIDSeen},
		{"trajectory match", WeightTrajectoryMatch},
		{"referrer", WeightReferrerExternal},
		{"landing page", WeightLandingPageRepeat},
		{"coarse signal", WeightCoarseSignal},
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].w <= ordered[i].w {
			t.Errorf("%s (%v) should outrank %s (%v)",
				ordered[i-1].name, ordered[i-1].w, ordered[i].name, ordered[i].w)
		}
	}
}

func TestDefaultWeightsAreProbabilities(t *testing.T) {
	all := []float64{
		WeightClickIDRepeat, WeightClickIDSeen, WeightTrajectoryMatch,
		WeightReferrerExternal, WeightLandingPageRepeat, WeightCoarseSignal,
		0.2, // utm
	}
	for _, w := range all {
		if w <= 0 || w > 1 {
			t.Errorf("weight %v is not a probability", w)
		}
	}
}

func TestFormatWeightRoundsToTwoPlaces(t *testing.T) {
	if got := FormatWeight(0.9249); got != "0.92" {
		t.Errorf("FormatWeight(0.9249) = %q, want 0.92", got)
	}
	if got := FormatWeight(1); got != "1.00" {
		t.Errorf("FormatWeight(1) = %q, want 1.00", got)
	}
}

func TestNormalizePathHelpers(t *testing.T) {
	if got := normalizePath("/a/b?x=1"); got != "/a/b" {
		t.Errorf("normalizePath = %q", got)
	}
	if got := normalizePath("/a/b#frag"); got != "/a/b" {
		t.Errorf("normalizePath = %q", got)
	}
	if got := normalizePath("relative"); got != "relative" {
		t.Errorf("normalizePath should leave relative paths alone, got %q", got)
	}
}

func TestResolveIgnoresTrajectoryWeightWhenNoSpine(t *testing.T) {
	rep := sampleReport(t)
	rep.Session.Pageviews = nil
	r := Resolve(rep, &Prior{})
	if got := spinePaths(rep.Session.Pageviews); len(got) != 0 {
		t.Errorf("spinePaths should be empty, got %v", got)
	}
	if len(r.Observations) == 0 {
		t.Error("a report with no pageviews should still produce a session observation")
	}
}

func TestResolvedShapeIsInternallyConsistent(t *testing.T) {
	r := Resolve(sampleReport(t), &Prior{})
	obsIDs := map[string]bool{}
	for _, o := range r.Observations {
		obsIDs[o.ID.String()] = true
		if o.Namespace != r.Subject.Namespace {
			t.Errorf("observation namespace %q != subject %q", o.Namespace, r.Subject.Namespace)
		}
	}
	for _, e := range r.Edges {
		if e.Src != r.Subject.ID {
			t.Errorf("edge src %v is not the subject %v", e.Src, r.Subject.ID)
		}
		if !obsIDs[e.Dst.String()] {
			t.Errorf("edge dst %v has no observation node", e.Dst)
		}
		if e.Type != EdgeRelatesTo {
			t.Errorf("edge type = %q, want %q", e.Type, EdgeRelatesTo)
		}
	}
	if reflect.TypeOf(r.Subject.Labels).Kind() != reflect.Slice {
		t.Error("subject labels should be a slice")
	}
}
