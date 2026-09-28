// Package identity reconstructs a visitor's identity from observations that
// live outside the URL query string.
//
// The premise: everyone already instruments the click. gclid, wbraid, fbclid,
// ttclid and the utm family arrive in the URL, are copied into GA4, and are
// the only linkage most teams have. Everything else that identifies a person
// -- the referrer chain, the landing page, the page sequence, dwell time,
// scroll depth, return visits, the gaps between visits -- is either not
// captured or is collapsed into an unverifiable session.
//
// This package treats identity as weighted edges between a subject and its
// observations, never as a merge decision. A node would force resolution at
// write time and destroy the uncertainty that makes identity hard: one person
// accumulates many identifiers over time (gclid rotates, cookies expire,
// sessions fragment), so any node-based scheme needs a resolution step whose
// output cannot be inspected or reversed. Edges preserve it. The resolution
// itself becomes a claim, with weight, that can be contested later.
//
// The client is not trusted. identity-helper.js reports what it saw and never
// concludes that two observations belong to the same person; that judgement
// lives here, in the graph, where it is auditable and can be revised.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/antiartificial/contextdb/internal/core"
)

// LabelSubject marks the node representing "the person we are accumulating
// evidence about". It is a hypothesis, not a person: a subject is invalidated
// and re-forged when contradicting evidence arrives.
const LabelSubject = "identity_subject"

// LabelObservation marks a single observation node (a pageview, a click-ID
// sighting, a coarse-signal sighting).
const LabelObservation = "identity_observation"

// EdgeRelatesTo links a subject to an observation, carrying the confidence
// that the observation belongs to that subject. This is the load-bearing edge:
// its Weight is the entire claim.
const EdgeRelatesTo = core.EdgeRelatesTo

// EdgeSourcedFrom links a subject to the evidence that produced it, so a
// derived belief can always be traced to raw observation.
const EdgeSourcedFrom = core.EdgeCites

// DefaultSessionTTL bounds how long a session-scoped observation stays
// relevant. A coarse device signal is evidence that two visits came from the
// same browser for a short while, and essentially no evidence after that.
const DefaultSessionTTL = 30 * time.Minute

// ClickIDParams are the advertising click identifiers that arrive in the URL.
// A sighting of one of these on a later visit is strong, deterministic
// evidence of continuity, because the platform minted it for that click.
var ClickIDParams = []string{
	"gclid", "gbraid", "wbraid", "dclid", "fbclid", "ttclid", "msclkid",
	"li_fat_id", "epik", "irclickid", "rdt_cid", "sccid", "yclid",
}

// UTMParams are campaign attribution parameters. Unlike click IDs these are
// self-reported by the marketer, so they are weak evidence of identity: one
// visitor can carry a dozen utm values across a session as they browse
// internally, and a bad CRM import can attach the same utm to thousands of
// unrelated people.
var UTMParams = []string{"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content"}

// Edge weights. These are deliberately coarse and are the first thing to
// recalibrate against a real corpus; they are not magic numbers, they are
// starting priors in the same spirit as core.Source's Laplace-smoothed
// Beta(1,1).
const (
	// WeightClickIDRepeat: the same advertising click ID was observed on two
	// separate visits. The platform minted that ID for a specific click, so
	// repeat observation is close to deterministic.
	WeightClickIDRepeat = 0.92

	// WeightClickIDSeen: a click ID observed once. Real, but a single
	// sighting is also consistent with link-shared or reloaded landing pages.
	WeightClickIDSeen = 0.70

	// WeightReferrerExternal: a referring host outside the site appeared in
	// the entry path of a returning visit.
	WeightReferrerExternal = 0.40

	// WeightLandingPageRepeat: the same entry path was seen before. Mild
	// evidence; popular landing pages are shared by unrelated visitors.
	WeightLandingPageRepeat = 0.35

	// WeightTrajectoryMatch: the page sequence substantially repeats. A path
	// is far more distinctive than any single page, which is the whole reason
	// the helper captures a spine rather than pageviews.
	WeightTrajectoryMatch = 0.55

	// WeightCoarseSignal: coarse device characteristics. Kept low on purpose
	// and paired with a short TTL: this is the same population of humans
	// behind every other visitor on a given browser, so on its own it is
	// nearly worthless. It exists to corroborate, never to identify.
	WeightCoarseSignal = 0.15
)

// Signal is the coarse, session-scoped device observation. Deliberately NOT a
// cross-site fingerprint: a stable per-browser identifier is precisely the
// category of thing being legislated out of existence, and building the
// product's identity primitive on it would make the product the thing that
// gets regulated. These fields are bucketed in the browser, salted per site,
// and expire with the session.
type Signal struct {
	ScreenBucket  string `json:"screen_bucket,omitempty"`
	LangBucket    string `json:"lang_bucket,omitempty"`
	TimezoneOff   int    `json:"timezone_offset,omitempty"`
	ConnType      string `json:"conn_type,omitempty"`
	HardwareCores int    `json:"hardware_cores,omitempty"`
	Platform      string `json:"platform,omitempty"`
}

// Pageview is one step of the session spine.
type Pageview struct {
	URL       string    `json:"url"`
	Path      string    `json:"path"`
	Referrer  string    `json:"referrer,omitempty"`
	Title     string    `json:"title,omitempty"`
	At        time.Time `json:"at"`
	DwellMS   int       `json:"dwell_ms,omitempty"`
	ScrollPct int       `json:"scroll_pct,omitempty"`
	ViewportW int       `json:"viewport_w,omitempty"`
	ViewportH int       `json:"viewport_h,omitempty"`
}

// Session is the client-reported spine for one visit.
type Session struct {
	ID            string     `json:"id"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       time.Time  `json:"ended_at"`
	EntryPath     string     `json:"entry_path"`
	EntryRef      string     `json:"entry_referrer,omitempty"`
	Pageviews     []Pageview `json:"pageviews,omitempty"`
	ReferrerChain []string   `json:"referrer_chain,omitempty"`
}

// Report is the ingest payload.
type Report struct {
	Site     string            `json:"site"`
	Subject  string            `json:"subject"`
	Session  Session           `json:"session"`
	ClickIDs map[string]string `json:"click_ids,omitempty"`
	UTM      map[string]string `json:"utm,omitempty"`
	Signal   Signal            `json:"signal,omitempty"`
	Consent  Consent           `json:"consent"`
	Agent    string            `json:"agent,omitempty"`
	// Mark is a deliberate signal pushed by the host site through
	// ctxdbIdentity.mark(), such as an identified-CRM match. It travels the
	// same pipeline as observed traffic rather than bolting on another
	// vendor, which is the point.
	Mark map[string]any `json:"mark,omitempty"`
}

// Consent is recorded rather than assumed. A refusal does not stop ingest of
// the session spine; it degrades the resolution so a site that is honest
// about opt-outs still gets useful linkage without pretending it has more.
type Consent struct {
	Analytics bool `json:"analytics"`
}

// Observation is one piece of evidence about a subject.
type Observation struct {
	ID          uuid.UUID
	Kind        string
	Weight      float64
	Detail      map[string]any
	ObservedAt  time.Time
	ValidUntil  *time.Time
	Fingerprint string
}

// Observation kinds.
const (
	KindPageview = "pageview"
	KindClickID  = "click_id"
	KindUTM      = "utm"
	KindReferrer = "referrer"
	KindSignal   = "coarse_signal"
	KindSession  = "session"
)

// Resolved is the graph-shaped output of one report: the subject node plus
// its observations and the weighted edges binding them.
type Resolved struct {
	Subject      core.Node
	Observations []core.Node
	Edges        []core.Edge
	Degraded     bool
}

// SubjectID derives a stable node ID from the site and subject key. Deriving
// rather than minting is what makes re-ingest idempotent: the same browser
// re-reporting produces the same subject node, and core.UpsertNode versions it
// instead of forking a duplicate. The site is part of the key so two sites in
// one contextdb deployment never cross-link.
func SubjectID(site, subject string) uuid.UUID {
	sum := sha256.Sum256([]byte("subject\x00" + normalizeSite(site) + "\x00" + subject))
	return uuidFromSum(sum)
}

// ObservationID derives a stable node ID for an observation. Including the
// subject key keeps two visitors who both saw /pricing from colliding into one
// node; including the kind and detail keeps a repeat sighting distinct from
// the first.
func ObservationID(site, subject, kind string, detail map[string]any) uuid.UUID {
	sum := sha256.Sum256([]byte("observation\x00" + normalizeSite(site) + "\x00" + subject +
		"\x00" + kind + "\x00" + canonicalDetail(detail)))
	return uuidFromSum(sum)
}

func uuidFromSum(sum [32]byte) uuid.UUID {
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4 shape, for readable dumps
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u
}

func normalizeSite(site string) string {
	s := strings.ToLower(strings.TrimSpace(site))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	return strings.TrimSuffix(s, "/")
}

// canonicalDetail renders a detail map deterministically so the same logical
// observation always hashes to the same ID.
func canonicalDetail(detail map[string]any) string {
	if len(detail) == 0 {
		return ""
	}
	keys := make([]string, 0, len(detail))
	for k := range detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%v\x1f", k, detail[k])
	}
	return b.String()
}

// Resolve turns a client report into graph writes. It makes no claim about who
// the person is; it records what was observed and how much each observation
// is worth as evidence of continuity.
func Resolve(r Report, prior *Prior) Resolved {
	site := normalizeSite(r.Site)
	now := r.Session.EndedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	out := Resolved{Degraded: !r.Consent.Analytics}

	subject := SubjectID(site, r.Subject)
	subjectNode := core.Node{
		ID:            subject,
		Namespace:     site,
		Labels:        []string{LabelSubject},
		Confidence:    0.5,
		EpistemicType: core.EpistemicObservation,
		ValidFrom:     firstNonZero(r.Session.StartedAt, now),
		TxTime:        now,
		Properties: map[string]any{
			"subject_key":   r.Subject,
			"consent":       r.Consent.Analytics,
			"agent":         r.Agent,
			"last_ingest":   now.Format(time.RFC3339),
			"session_count": prior.Sessions + 1,
			"degraded":      out.Degraded,
		},
	}
	out.Subject = subjectNode

	emit := func(kind string, detail map[string]any, weight float64, observedAt time.Time, ttl *time.Time) {
		obs := Observation{
			ID:          ObservationID(site, r.Subject, kind, detail),
			Kind:        kind,
			Weight:      weight,
			Detail:      detail,
			ObservedAt:  observedAt,
			ValidUntil:  ttl,
			Fingerprint: fingerprintOf(kind, detail),
		}
		out.Observations = append(out.Observations, core.Node{
			ID:            obs.ID,
			Namespace:     site,
			Labels:        []string{LabelObservation},
			Confidence:    obs.Weight,
			EpistemicType: core.EpistemicObservation,
			ValidFrom:     firstNonZero(observedAt, now),
			ValidUntil:    obs.ValidUntil,
			TxTime:        now,
			Fingerprint:   obs.Fingerprint,
			Properties: map[string]any{
				"kind":       kind,
				"weight":     obs.Weight,
				"detail":     detail,
				"degraded":   out.Degraded,
				"session_id": r.Session.ID,
			},
		})
		out.Edges = append(out.Edges, core.Edge{
			Namespace:  site,
			Src:        subject,
			Dst:        obs.ID,
			Type:       EdgeRelatesTo,
			Weight:     obs.Weight,
			ValidFrom:  firstNonZero(observedAt, now),
			ValidUntil: obs.ValidUntil,
			TxTime:     now,
			Properties: map[string]any{"kind": kind, "degraded": out.Degraded},
		})
	}

	// The session spine. Emitted as a single observation because the sequence
	// is the signal: no single pageview identifies anybody, but a four-step
	// path through docs back to pricing is close to distinctive.
	spine := map[string]any{
		"session_id": r.Session.ID,
		"entry_path": r.Session.EntryPath,
		"steps":      len(r.Session.Pageviews),
		"duration_s": int(r.Session.EndedAt.Sub(r.Session.StartedAt).Seconds()),
		"paths":      spinePaths(r.Session.Pageviews),
	}
	emit(KindSession, spine, 1.0, firstNonZero(r.Session.StartedAt, now), nil)

	// Click IDs: strong, and the only deterministic link available.
	for _, param := range ClickIDParams {
		v, ok := r.ClickIDs[param]
		if !ok || v == "" {
			continue
		}
		weight := WeightClickIDSeen
		if prior.SeenClickIDs[param+":"+v] {
			weight = WeightClickIDRepeat
		}
		emit(KindClickID, map[string]any{"param": param, "value": v}, weight, r.Session.EndedAt, nil)
	}

	// UTM: weak. Carried for campaign context, not as identity evidence.
	for _, param := range UTMParams {
		v, ok := r.UTM[param]
		if !ok || v == "" {
			continue
		}
		emit(KindUTM, map[string]any{"param": param, "value": v}, 0.2, r.Session.EndedAt, nil)
	}

	// Referrer chain. An external referrer on a return visit is mild evidence
	// of the same person coming back from the same place.
	for _, ref := range r.Session.ReferrerChain {
		if ref == "" || isSelfRef(ref, site) {
			continue
		}
		weight := WeightReferrerExternal
		if prior.SeenReferrers[ref] {
			weight = WeightReferrerExternal + 0.15
		}
		emit(KindReferrer, map[string]any{"host": ref}, weight, r.Session.EndedAt, nil)
	}

	// Coarse signal: low weight, session-scoped validity. Expiring it with the
	// session is what stops a weak signal from accumulating into a persistent
	// identifier by repetition.
	if r.Signal.ScreenBucket != "" || r.Signal.LangBucket != "" {
		expiry := now.Add(DefaultSessionTTL)
		emit(KindSignal, map[string]any{
			"screen":    r.Signal.ScreenBucket,
			"lang":      r.Signal.LangBucket,
			"tz_offset": r.Signal.TimezoneOff,
			"conn":      r.Signal.ConnType,
			"cores":     r.Signal.HardwareCores,
			"platform":  r.Signal.Platform,
		}, WeightCoarseSignal, now, &expiry)
	}

	return out
}

// Prior is what the graph already knows about this subject, supplied by the
// caller. Keeping it a parameter rather than reading the store here is what
// lets Resolve stay pure and testable.
type Prior struct {
	Sessions       int
	SeenClickIDs   map[string]bool
	SeenReferrers  map[string]bool
	TrajectorySeen bool
}

func spinePaths(pvs []Pageview) []string {
	out := make([]string, 0, len(pvs))
	for _, pv := range pvs {
		if pv.Path != "" {
			out = append(out, pv.Path)
		}
	}
	return out
}

func fingerprintOf(kind string, detail map[string]any) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + canonicalDetail(detail)))
	return hex.EncodeToString(sum[:])[:32]
}

func isSelfRef(ref, site string) bool {
	host := ref
	if i := strings.Index(ref, "://"); i >= 0 {
		host = ref[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	return strings.EqualFold(strings.TrimPrefix(host, "www."), site)
}

func firstNonZero(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
