package server

import (
	"net/http"

	"github.com/antiartificial/contextdb/internal/identity"
)

// registerIdentityRoutes mounts the identity helper tag and its ingest
// endpoint. Both are served by this binary, so adopting the helper adds no
// third-party script origin, no CDN dependency, and no build step on the
// customer's site.
func (s *RESTServer) registerIdentityRoutes(mux *http.ServeMux) {
	graph, _, _, _ := s.db.Stores()
	h := identity.NewHandler(graph, identity.GraphPrior(graph))

	// GET /identity-helper.js
	mux.HandleFunc("GET /identity-helper.js", h.ServeTag)

	// POST /v1/identity/ingest
	mux.HandleFunc("POST /v1/identity/ingest", h.Ingest)
	mux.HandleFunc("OPTIONS /v1/identity/ingest", h.Ingest)
}
