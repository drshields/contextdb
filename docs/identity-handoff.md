# Handoff: contextdb identity work

Written for a fresh agent or engineer with **no context on how this started.**
If you are reading this cold, the sections below are the whole state of play.

## What you are looking at

`contextdb` is a temporal graph-vector database that models *epistemic* claims:
things have sources, sources have credibility, facts contradict each other and
get superseded, and beliefs are versioned and auditable. It is pre-1.0, Go,
single binary, no server dependencies. Upstream is `antiartificial/contextdb`;
this fork is `drshields/contextdb`.

The branch `feat/identity-helper` adds an identity subsystem: a client tag plus
ingest that reconstructs who a visitor is from evidence *outside* the URL query
string. It is a working draft, deliberately incomplete, and it has a known
missing half described in "The gap" below.

Read `docs/identity-helper.md` for the model and the privacy posture. This file
covers only what the doc cannot tell you.

## Why this exists, in one paragraph

Digital analytics instruments the *click*. `gclid`, `gbraid`, `wbraid`,
`fbclid`, `ttclid` and the `utm` family arrive in the query string, get copied
into GA4, and become the only linkage most implementations have. Those values
describe one pageview on one landing. They say nothing about who landed, where
they went, or whether they came back. The referrer chain, the page sequence,
dwell, scroll and the gaps between return visits are not collected at all.

The larger ambition is reconciliation: multiple platforms assert contradictory
claims about the same business fact (GA4 says 412 conversions, Shopify says 389,
Meta claims credit for all of them), and today's answer is an ETL `MERGE` that
silently overwrites. This project wants to keep both claims, mark the
supersession, and let credibility decide — so the number can be *defended*.

## The design position, in three claims

**1. The client never concludes.** `identity-helper.js` reports what it
observed. `internal/identity` decides what it means. This is the load-bearing
constraint: a tag that decides identity is a tag that can lie about it, and an
untrusted client is what makes the system auditable.

**2. Identity is weighted edges, never a merge decision.** A node forces
resolution at write time and destroys the uncertainty that makes identity hard.
One person accumulates many identifiers over time — `gclid` rotates, cookies
expire, sessions fragment — so any node scheme needs a resolution step whose
output cannot be weighted or reversed. Edges make the resolution itself a
contestable claim.

**3. Evidence strength is a function of who minted the claim, not how much data
sits behind it.** A platform minted that `gclid` knowing which click it was. A
browser is one of millions behind an IP. A `utm` value was typed by a marketer
and a bad CRM import can attach one to thousands of people. The weight ordering
in `model.go` *is* the model; `TestEdgeWeightOrdering` pins it so recalibrating
stays a deliberate act rather than a drift.

## Deliberate non-goals — do not "fix" these

- **No cross-site fingerprinting.** No canvas, audio, font or plugin
  enumeration. A stable cross-site browser identifier is the category being
  legislated out of existence, and an identity product built on one becomes the
  thing that gets regulated. A test asserts those primitives are absent from the
  served tag. Coarse signals are bucketed, salted per site, and expire with the
  session — a weak signal that never expires accumulates into a persistent
  identifier by repetition.
- **No third-party cookies.** First-party only, `SameSite=Lax` so an external
  referrer still carries the key.
- **No cross-property stitching.** See `docs/identity-helper.md`. Linking one
  person across two unrelated domains is not possible without an identifier you
  should not be shipping. The correct behaviour is to report that the linkage
  was unavailable, not to invent a number.
- **The browser is never trusted**, including `mark()`. A mark starts at 0.50
  and promotes once to 0.85. The cap is the point: repetition is not
  independence, and a browser must not be able to outrank a platform-minted
  click id by asserting the same thing often enough.

## The gap: there is no consumer

**This is the most important thing in this file.**

The identity graph writes evidence. It has no user-facing output. The
consumption surface already exists in this repo — `narrative`, `consensus`,
`explain`, evidence chains, citations — and nothing connects them.

The intended UX is an **argument surface, not a dashboard**. The user arrives
holding a number they cannot defend ("attributed conversions yesterday: 412").
What they lack is not another chart but the reasoning: which 23 claims we
counted, which 4 we excluded, what outranked each of them, and what would put
them back. The unit of the interface is a *disagreement*, not a metric.

Do not build a monitoring dashboard. Build the argument.

## Known gaps, in priority order

1. **No consumption surface.** See above. Everything else is secondary.
2. **Trajectory weighting is unwired.** `TrajectorySimilarity` and
   `WeightForTrajectory` in `prior.go` are written and tested but never called
   by `GraphPrior`, which only reads click IDs and referrers.
3. **The weights are priors, not measurements.** They need a real corpus.
4. **The corroborated-mark ceiling of 0.85 is a placeholder** for an
   independence mechanism that does not exist. Anyone repeating a mark enough
   times is currently treated as evidence, which is a hole.
5. **`registrableDomain()` ships a short public-suffix list,** not the full PSL.
   `data-ctxdb-site` is the escape hatch.

## Environment

Go is **not on the default PATH**. It is installed at `~/.local/go`:

```bash
export PATH="$HOME/.local/go/bin:$PATH"
go version   # go1.26.8
```

Toolchain is pinned by `go.mod` (`go 1.26.0` + `toolchain go1.26.8`) and CI
reads it via `go-version-file: go.mod`, so it cannot drift. `gofmt` reports 23
pre-existing unformatted files inherited from upstream; they are not yours and
`make lint` is only `go vet`.

Admin UI: `npm ci && npm run admin:build` before anything that needs the
embedded UI. A clean checkout builds without it, but dashboard *content* tests
skip until the build runs.

## Things that will bite you

- **Tests built in Go do not exercise the wire format.** Two shipped bugs came
  from this: a dead `mark()` API, and `referrer_host` being sent by the tag but
  undeclared in Go, which made *every real ingest return 400* under
  `DisallowUnknownFields`. `internal/identity/contract_test.go` now carries a
  verbatim copy of the real tag payload for this reason. When you change the
  tag, change that constant too.
- **`emit()` takes observation detail and edge properties separately, on
  purpose.** Derived state (corroboration counts) belongs on the edge. Putting
  it in the detail map also puts it in the hash input, so every update mints a
  new observation node and fragments the evidence.
- **Lower-confidence edges are retained on purpose.** A mark held at 0.5 and
  later promoted to 0.85 leaves both edges. Read the *strongest active*
  `relates_to` per observation; do not sum them.
- **CORS preflight is handled before the method check** in `Ingest`. Reordering
  those breaks the tag, which is loaded cross-site by design.

## What this was, contextually

The identity work and a separate effort (reconciling cross-source analytics
claims) are the same idea applied twice: never resolve disagreement by hiding
it, retain both claims, mark the supersession, attribute it, and make it
checkable. `EdgeSupersedes` and a transparency log are the same move.

That thread is documented in `docs/roadmap/` only partially. The reasoning is
not in the repo.
