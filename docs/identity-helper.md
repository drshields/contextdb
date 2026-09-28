# Identity Helper

A companion tag for `gtag.js`. It reconstructs who someone is from evidence
that lives outside the URL query string.

## Why this exists

Every team already instruments the click. `gclid`, `gbraid`, `wbraid`,
`fbclid`, `ttclid` and the `utm` family arrive in the query string, get copied
into GA4, and become the only linkage most implementations have. Those values
describe **one pageview on one landing**. They say nothing about the person who
landed, where they went next, or whether they came back.

Everything else that identifies a visitor is not collected:

- the referrer chain behind the click
- the landing page itself
- the page sequence through the site
- dwell time and scroll depth
- return visits and the gaps between them

That is the signal this tag collects, and the graph weighs it.

## Install

```html
<script async
        src="https://your-contextdb-host/identity-helper.js"
        data-ctxdb-endpoint="https://your-contextdb-host/v1/identity/ingest"></script>
```

That is the whole integration. Load it before or after `gtag.js`; the two do not
interact and order does not matter. The tag is served by the contextdb binary,
so there is no third-party script origin, no CDN, and no build step on the
customer's site.

Optional attributes:

| Attribute | Default | Purpose |
|---|---|---|
| `data-ctxdb-endpoint` | `/v1/identity/ingest` | where reports are sent |
| `data-ctxdb-site` | `location.hostname` | overrides the site key |

## What the tag sends

One report per session, fired on `visibilitychange` and `pagehide`, using
`navigator.sendBeacon` so the most interesting pageviews (the ones that end a
session) survive page unload.

```jsonc
{
  "site": "shop.example.com",
  "subject": "first-party-uuid-in-a-cookie",
  "session": {
    "id": "session-uuid",
    "started_at": "...", "ended_at": "...",
    "entry_path": "/landing",
    "entry_referrer": "https://news.ycombinator.com/...",
    "pageviews": [ { "path": "/landing", "dwell_ms": 8200, "scroll_pct": 100, ... } ],
    "referrer_chain": ["news.ycombinator.com"]
  },
  "click_ids": { "gclid": "...", "wbraid": "..." },
  "utm":       { "utm_source": "reddit" },
  "signal":    { "screen_bucket": "1a2b3c", "lang_bucket": "en-US", "timezone_offset": -360 },
  "consent":   { "analytics": true }
}
```

`window.ctxdbIdentity.mark(kind, detail)` pushes a deliberate signal through the
same pipeline — an identified CRM match, a consent change, a support chat —
instead of bolting on another vendor.

## The model: identity as weighted edges

Identity is a **set of weighted edges**, never a merge decision.

A node would force resolution at write time, which destroys the uncertainty that
makes identity hard in the first place. One person accumulates many identifiers
over time: `gclid` rotates, cookies expire, sessions fragment, devices multiply.
Any node-based scheme therefore needs a resolution step, and that step's output
cannot be inspected, weighted, or reversed. Edges preserve the uncertainty. The
resolution itself becomes a claim, with a weight, that can be contested later.

The client is **not trusted**. The tag reports what it observed; it never
concludes that two observations belong to the same person. That judgement lives
in `internal/identity`, where it is testable and auditable. An untrusted client
is what makes this deployable at all — a tag that decides identity is a tag that
can lie about it.

### Weights

| Evidence | Weight | Why |
|---|---|---|
| Advertising click ID, seen again | **0.92** | The platform minted that ID for a specific click. Repeat observation is near-deterministic. |
| Advertising click ID, first sighting | **0.70** | Real, but one sighting is also consistent with a shared or reloaded link. |
| Trajectory match | **0.55** | A page sequence is far more distinctive than any single page. |
| External referrer on a return visit | **0.40** | Mild; the same blog sends many unrelated people. |
| Repeated landing path | **0.35** | Popular pages are shared by everyone. |
| Coarse device signal | **0.15** | Weak corroboration only. Expires with the session. |
| `utm` parameter | **0.20** | Carried for campaign context, explicitly **not** as identity evidence. |

These are starting priors in the same spirit as `core.Source`'s
Laplace-smoothed `Beta(1,1)`. They are the first thing to recalibrate against a
real corpus, and `TestEdgeWeightOrdering` pins their relative ranking so a
recalibration is a deliberate act rather than an accident.

## Privacy position

This is the part worth arguing with, so it is stated plainly.

**No third-party cookies.** Not required and increasingly blocked. The subject
key is a first-party cookie set by this site, `SameSite=Lax` so a click from an
external referrer still carries it — which is exactly the return visit that
matters.

**No cross-site fingerprint.** The coarse signals are bucketed in the browser,
salted per site, and scoped to the session. There is no canvas, audio, font, or
plugin enumeration; a test asserts their absence from the served tag. A stable
per-browser identifier is precisely the category of thing being legislated out of
existence, and an identity product built on one becomes the thing that gets
regulated. Weak signal that expires is worth more here than strong signal that
cannot be deployed.

**Degradation, not suppression.** Withheld consent marks the record `degraded`
and still ingests the first-party session spine. Blocking outright would mean
pretending we have no linkage when we do, which is a worse record than
recording that consent was withheld.

**Nothing new leaves the browser.** The tag does not read page content, form
fields, or keystrokes. It observes navigation the page already exposes.

## What it does not do

- Replace `gtag.js` or declare a second source of truth
- Decide who anyone is
- Survive a cleared cookie as a persistent identifier
- Work across sites

## Endpoints

| Route | Purpose |
|---|---|
| `GET /identity-helper.js` | the tag, served from the binary |
| `POST /v1/identity/ingest` | accepts one report |

Ingest rejects unknown fields rather than silently accepting them: the client is
untrusted, so the decoder refuses to absorb surface it does not understand.

## Next

- `GraphPrior` currently reads click IDs and referrers. It should also compare
  the incoming spine against stored spines and weight by `TrajectorySimilarity`.
- The weights above are priors, not measurements. They want a real corpus.
- Inter-site stitching (the same person across `landing.` and `checkout.`) is
  the case that motivates the whole design and is not yet implemented.
