# GraphQL operation metrics — body peek

`graphql.Metrics` can label series by `operationName`. That name usually lives
in the JSON POST body. Reading every body in full would be an easy DoS vector
on a public `/graphql` endpoint, so extraction is **gated and bounded**.

## The tradeoff

Operation-level metrics need a name. Over HTTP that name is almost never in
headers — it is inside the POST JSON (`operationName`) or the query string on
GET. So the middleware must **look at the request** to label the series.

| You want | You pay |
|---|---|
| Named series (`GetUser`, allowlist, `other`) | Some CPU + a bounded body peek (or a full read if you opt in) on GraphQL-shaped POSTs |
| Zero body touch / zero parse risk from this middleware | Leave tracking **off** (default), or supply the name yourself (`WithOperationName` / library hooks) so Metrics never has to parse |
| Perfect labels even when `operationName` is late in a huge body | `WithPeekWindow(0)` — full buffer before the handler; **worst** cost profile |

What we optimize for by default (tracking **on**, peek **unset**):

- **Correct enough labels** for normal GraphQL-over-HTTP clients (name near
  the start of the JSON).
- **Hard upper bound** on work per request (Content-Type gate + 8 KiB peek +
  cheap structure check) so a flood of large POSTs cannot force megabyte
  JSON parses in this middleware.
- **Honest failure mode** when the name is outside the window: empty /
  `anonymous` / drop / `other` — not invented labels, not a second full-body
  scan.

What we deliberately do **not** promise:

- Parsing every possible GraphQL encoding (multipart uploads, custom
  content types) — those skip extraction.
- Cardinality safety if you enable `AllOperations()` — that is a separate,
  documented footgun.
- That the GraphQL **handler** itself is DoS-proof — put `MaxBodyBytes` on
  the GraphQL POST route (or a proxy limit) separately; this doc is only
  about **metrics extraction** cost. Peek window and body limit are not
  the same setting.

Practical recommendation: keep the default peek, put `operationName` first in
client payloads (or set it in context), use `OnlyOperations`, and reserve
`WithPeekWindow(0)` for trusted/internal graphs.

## Label: `graphql_instrumentation`

Every `http_graphql_operations_*` / `http_graphql_resolvers_*` sample carries
how the name was obtained:

| Value | Meaning |
|---|---|
| **`http_peek`** | Recorded by `graphql.Metrics` auto body-peek middleware (convenience path) |
| **`app`** | Recorded by application / engine hooks (`RecordGraphQLMetric`, `StartOperation`, `RecordResolver`) |

Use this in **dev / staging** to spot forgotten migration off auto-peek:

```promql
# Anything still coming from HTTP body peek:
http_graphql_operations_total{graphql_instrumentation="http_peek"}
```

Production graphs that have moved to engine instrumentation should show only
`graphql_instrumentation="app"` (or no GraphQL operation series at all). Seeing
`http_peek` in prod is a signal to finish wiring library hooks / persisted
ops — not a hard error, but intentional visibility.

## Alternatives — metrics without auto body peek

If you do not want the HTTP middleware to inspect bodies, you can still emit
the same series by supplying the name from code that already parsed the
operation (the GraphQL server / your resolvers).

| Approach | How | Body peek needed? |
|---|---|---|
| **Context name** | After your stack knows the op, `ctx = graphql.WithOperationName(ctx, name)` (and keep `OnlyOperations` if you still wrap with `Metrics` for timing — or skip `Metrics` and record yourself) | No, if you never enable auto extract / leave tracking off |
| **Explicit record** | `graphql.StartOperation(server, name)` … `End(status)` around the handler, or `server.RecordGraphQLMetric(name, status, d)` | No |
| **Resolver hooks** | `graphql.RecordResolver(server, op, field, status, d)` from library field middleware | No |
| **Persisted queries / allowlist IDs** | Client sends a hash/ID; server maps ID → known operation name, then records that name | No parse of the full query document for metrics |
| **Skip operation series** | Rely on ordinary `cf_http.Metrics` route labels (`/graphql` only; `http_instrumentation=middleware` — that REST path is industry-normal, unlike `http_peek`) | No |

Sketch (library already parsed the op — no `graphql.Metrics` body path):

```go
// Inside your GraphQL around-operation hook (gqlgen extension, etc.):
op := graphql.StartOperation(httpServer, operationName) // from execution context
defer op.End(http.StatusOK) // or status derived from GraphQL errors

// Or one-shot:
// httpServer.RecordGraphQLMetric(operationName, status, duration)
```

Recommended production shape for many teams: **persisted/allowlisted operations**
+ **instrument inside the GraphQL engine** (name from the AST / op context), and
use `graphql.Metrics` auto-peek only as a convenience for simple apps.

## What the industry usually does

- **Instrument inside the GraphQL server**, not by re-parsing the raw HTTP body
  in a generic middleware. gqlgen / Apollo / similar expose operation name and
  type on the execution context after they have parsed (or loaded) the document.
  OpenTelemetry GraphQL conventions (`graphql.operation.name`,
  `graphql.operation.type`) assume that path (e.g. `otelgqlgen` and peers).
- **Prefer allowlists or persisted queries** for anything customer-facing:
  clients send a known name or query hash; servers reject unknown ops. That
  caps cardinality and avoids treating free-form `operationName` as a metric
  label (Apollo APQ / persisted query catalogs are the common pattern).
- **HTTP-layer metrics stay coarse** (`POST /graphql` latency/status). Fine
  operation breakdown is an application/GraphQL concern.
- Auto body peek (what `graphql.Metrics` offers) is a **convenience for
  handlers that are opaque `http.Handler`s** — useful, but not the usual
  “serious” production instrumentation path.

## When the body is touched at all

| Mode | Body read? |
|---|---|
| `Metrics(server)` (default — no tracking) | **Never** |
| Tracking on (`OnlyOperations` / `AllOperations` / `WithOtherBucket`) | Only after the checks below |

## POST extraction pipeline (tracking on)

1. **Content-Type preflight** — must be GraphQL-ish:
   `application/json`, `application/graphql`, or `application/graphql+json`
   (parameters like `charset` ignored). Anything else → no body touch.
2. **Peek window** — inspect a leading slice of the body (see
   [`WithPeekWindow`](#withpeekwindow)), restore it for the handler.
3. **Structure check** — the inspected bytes must look like a GraphQL JSON
   object: after whitespace, `{`, and at least one of `"query"`,
   `"operationName"`, `"extensions"` in the window. Otherwise → stop; no
   further reads.
4. **Name extract** — `json.Unmarshal` when the window is complete JSON; if
   truncated, a tight regex still accepts a complete
   `"operationName":"..."` string inside the window.

GET requests never read a body: `operationName` / `query` come from the URL.

## `WithPeekWindow`

Optional. Controls how many leading POST bytes step 2 may inspect.

| Setting | Meaning |
|---|---|
| **Omitted** (not set) | **`DefaultPeekWindow` (8 KiB)** — recommended for public endpoints |
| **`n > 0`** | Peek at most `n` bytes |
| **`0`** | **Full body read** for extraction (explicit opt-in) |

```go
// Default 8 KiB peek (safe).
graphql.Metrics(srv, graphql.OnlyOperations("GetUser"))

// Larger peek if your clients put a big preamble before operationName.
graphql.Metrics(srv,
    graphql.OnlyOperations("GetUser"),
    graphql.WithPeekWindow(32<<10), // 32 KiB
)

// Full read — only when you accept the cost (see below).
graphql.Metrics(srv,
    graphql.OnlyOperations("LateOp"),
    graphql.WithPeekWindow(0),
)
```

`WithMaxBodySize` is an alias for `WithPeekWindow`.

## What if the name is outside the window?

If `operationName` (or the GraphQL shape keys) sit **beyond** the peek window:

- Default / `n > 0`: extraction returns empty → the request is treated as
  **`anonymous`** for allowlist purposes (or dropped / `other`, depending on
  options). Labels are not guessed from garbage.
- Prefer putting `operationName` near the start of the JSON (normal
  GraphQL-over-HTTP clients do), **or** set the name in context
  (`WithOperationName`) / use library hooks, **or** set
  `WithPeekWindow(0)` if you truly need a full scan.

Incomplete JSON inside the window is fine: a full unmarshal may fail, but a
complete `"operationName":"..."` string still counts.

## Why `0` (full read) is opt-in

`WithPeekWindow(0)` buffers the **entire** body before the handler runs
(then restores it). That re-introduces the large-POST CPU/memory cost the
peek exists to avoid. Use it only behind auth, size limits at the proxy, or
when payloads are known-small. Prefer a slightly larger peek (`32<<10`) over
`0` when the name is merely “a bit late.”

## Related

- [examples.md](examples.md) — wiring sketch  
- Module README → “GraphQL operation metrics”
