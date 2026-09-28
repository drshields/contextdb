package identity

import (
	"context"
	"strconv"

	"github.com/antiartificial/contextdb/internal/store"
)

// GraphPrior reads what is already known about a subject so a new report can
// be weighted against it. Without this every sighting is a first sighting and
// the repeat-detection that makes click IDs valuable never fires.
func GraphPrior(graph store.GraphStore) PriorFunc {
	return func(ctx context.Context, site, subject string) (*Prior, error) {
		prior := &Prior{
			SeenClickIDs:  map[string]bool{},
			SeenReferrers: map[string]bool{},
			SeenMarks:     map[string]int{},
		}
		if graph == nil {
			return prior, nil
		}
		subjectID := SubjectID(site, subject)

		edges, err := graph.EdgesFrom(ctx, site, subjectID, nil)
		if err != nil {
			return prior, err
		}
		for _, e := range edges {
			obs, err := graph.GetNode(ctx, site, e.Dst)
			if err != nil || obs == nil {
				continue
			}
			kind, _ := obs.Properties["kind"].(string)
			detail, _ := obs.Properties["detail"].(map[string]any)
			switch kind {
			case KindClickID:
				param, _ := detail["param"].(string)
				value, _ := detail["value"].(string)
				if param != "" && value != "" {
					prior.SeenClickIDs[param+":"+value] = true
				}
			case KindReferrer:
				if host, _ := detail["host"].(string); host != "" {
					prior.SeenReferrers[host] = true
				}
			case KindMark:
				kind, _ := detail["kind"].(string)
				markDetail, _ := detail["detail"].(map[string]any)
				if kind != "" {
					prior.SeenMarks[MarkKey(kind, markDetail)]++
				}
			case KindSession:
				prior.Sessions++
			}
		}
		return prior, nil
	}
}

// TrajectorySimilarity scores how much a new page sequence resembles one
// already seen, in [0,1]. It is exported because the honest answer to "is this
// the same person" is a function over paths, not a function over any single
// page, and the caller usually has the previous path rather than the graph.
//
// This is a Jaccard overlap over normalized path segments. It is deliberately
// crude: a real implementation should weight by dwell time and by the
// rareness of each path segment, since / and /pricing are visited by everyone
// and are worth almost nothing on their own.
func TrajectorySimilarity(previous, current []string) float64 {
	if len(previous) == 0 || len(current) == 0 {
		return 0
	}
	a := make(map[string]struct{}, len(previous))
	for _, p := range previous {
		a[normalizePath(p)] = struct{}{}
	}
	shared := 0
	for _, p := range current {
		if _, ok := a[normalizePath(p)]; ok {
			shared++
		}
	}
	union := len(previous) + len(current) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

func normalizePath(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return trimIndex(p)
	}
	return p
}

func trimIndex(p string) string {
	for i := 1; i < len(p); i++ {
		if p[i] == '?' || p[i] == '#' {
			return p[:i]
		}
	}
	return p
}

// WeightForTrajectory returns the relates_to weight for a repeated trajectory,
// floored at the base trajectory weight so a strong path match never scores
// worse than a weak one.
func WeightForTrajectory(similarity float64) float64 {
	if similarity <= 0 {
		return 0
	}
	w := WeightTrajectoryMatch * similarity
	if w < WeightLandingPageRepeat {
		return WeightLandingPageRepeat
	}
	return w
}

// FormatWeight renders a weight for logs and receipts without implying more
// precision than the model has.
func FormatWeight(w float64) string {
	return strconv.FormatFloat(w, 'f', 2, 64)
}
