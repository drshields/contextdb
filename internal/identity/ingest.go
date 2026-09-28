package identity

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/antiartificial/contextdb/internal/core"
	"github.com/antiartificial/contextdb/internal/store"
)

//go:embed identity-helper.js
var helperJS string

// HelperJS returns the client tag. It is served from the Go binary so a
// deployment stays a single artifact with no CDN, no build step for the
// customer's site, and no third-party script origin.
func HelperJS() string { return helperJS }

// MaxReportBytes caps the ingest body. A session spine is small; anything
// larger is a bug or an abuse attempt.
const MaxReportBytes = 32 << 10

// PriorFunc supplies what is already known about a subject so new
// observations can be weighted against it.
type PriorFunc func(ctx context.Context, site, subject string) (*Prior, error)

// Handler ingests client reports and writes the resulting graph.
type Handler struct {
	graph store.GraphStore
	prior PriorFunc
}

// NewHandler builds an ingest handler. prior may be nil, in which case every
// report is weighted as a first sighting.
func NewHandler(graph store.GraphStore, prior PriorFunc) *Handler {
	if prior == nil {
		prior = func(context.Context, string, string) (*Prior, error) { return &Prior{}, nil }
	}
	return &Handler{graph: graph, prior: prior}
}

// ServeTag serves the client script.
func (h *Handler) ServeTag(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(helperJS))
}

// Ingest accepts one report. It is deliberately forgiving of shape and strict
// about provenance: unknown fields are dropped, but nothing is inferred.
func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	// Preflight first: the tag is loaded cross-site by design, so a CORS
	// preflight reaches this handler and must not be rejected as a bad method.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "content-type")
	w.Header().Set("Access-Control-Max-Age", "600")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var report Report
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxReportBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		http.Error(w, "malformed report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validate(&report); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	prior, err := h.prior(r.Context(), normalizeSite(report.Site), report.Subject)
	if err != nil {
		http.Error(w, "prior lookup failed", http.StatusInternalServerError)
		return
	}
	resolved := Resolve(report, prior)

	if err := h.graph.UpsertSource(r.Context(), sourceFor(resolved, report)); err != nil {
		http.Error(w, "source write failed", http.StatusInternalServerError)
		return
	}
	if err := h.graph.UpsertNode(r.Context(), resolved.Subject); err != nil {
		http.Error(w, "subject write failed", http.StatusInternalServerError)
		return
	}
	for _, obs := range resolved.Observations {
		if err := h.graph.UpsertNode(r.Context(), obs); err != nil {
			http.Error(w, "observation write failed", http.StatusInternalServerError)
			return
		}
	}
	for _, edge := range resolved.Edges {
		if err := h.graph.UpsertEdge(r.Context(), edge); err != nil {
			http.Error(w, "edge write failed", http.StatusInternalServerError)
			return
		}
	}

	ids := make([]string, 0, len(resolved.Observations))
	for _, obs := range resolved.Observations {
		ids = append(ids, obs.ID.String())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"subject":      resolved.Subject.ID.String(),
		"observations": ids,
		"edges":        len(resolved.Edges),
		"degraded":     resolved.Degraded,
	})
}

// sourceFor registers the browser agent as an observation-class source. Its
// credibility is deliberately mediocre: it is a client-side reporter that can
// be tampered with, and the credibility model should reflect that rather than
// treating client telemetry as fact.
func sourceFor(resolved Resolved, report Report) core.Source {
	return core.Source{
		ExternalID: report.Agent,
		Labels:     []string{"client_side_agent", "unverified"},
		Properties: map[string]any{
			"domain_cred": map[string]any{
				"identity_observation": map[string]any{
					"alpha": 1.0,
					"beta":  1.0,
				},
			},
			"degraded": resolved.Degraded,
		},
	}
}

func validate(r *Report) error {
	if strings.TrimSpace(r.Site) == "" {
		return errValidation("site is required")
	}
	if strings.TrimSpace(r.Subject) == "" {
		return errValidation("subject is required")
	}
	if strings.TrimSpace(r.Session.ID) == "" {
		return errValidation("session.id is required")
	}
	if r.Session.EndedAt.IsZero() {
		r.Session.EndedAt = time.Now().UTC()
	}
	if r.Session.StartedAt.IsZero() {
		r.Session.StartedAt = r.Session.EndedAt
	}
	if r.Session.EndedAt.Before(r.Session.StartedAt) {
		return errValidation("session.ended_at precedes session.started_at")
	}
	if r.Session.EntryPath == "" && len(r.Session.Pageviews) > 0 {
		r.Session.EntryPath = r.Session.Pageviews[0].Path
	}
	return nil
}

type errValidation string

func (e errValidation) Error() string { return string(e) }
