# connectors

Spec-driven API connectors for AI agents, in Go.

An app's OpenAPI document plus a short manifest becomes a set of tools a
model can call: each operation gets a name, a description and a JSON Schema
for its arguments, and `Execute` sends the call to the app with the person's
credential attached the way the manifest says. No hosted middleman, no
per-app code.

```go
m, _ := manifests.Get("bluepillow")
c, _ := connectors.Load(ctx, m)

creds := connectors.Credentials{"api_key": key}
if err := c.Check(ctx, creds); err != nil { /* ask again */ }

for _, t := range c.Tools() { /* t.Name, t.Description, t.Schema, t.ReadOnly */ }
r, err := c.Execute(ctx, creds, "BLUEPILLOW_RESOLVE_DESTINATION", json.RawMessage(`{"name":"Paris"}`))
fmt.Println(r.Status, r.String())
```

## Why a manifest

An OpenAPI document describes the operations well and the auth badly. Blue
Pillow's document, for example, declares no `securitySchemes` at all, though
every call needs `Authorization: Bearer <key>`; a generator that trusts the
document sends the key bare and gets `401 No API key provided`. The manifest
is where that knowledge lives:

```yaml
slug: bluepillow
name: Blue Pillow Hotels & Stays
spec: https://api.b2a.bluepillow.com/v1/openapi.json
base_url: https://api.b2a.bluepillow.com
auth:
  type: bearer            # bearer | header | query | basic | none
  fields:
    - {name: api_key, label: B2A API key, public: true}
check:                    # a cheap authenticated request
  path: /v1/keys/{api_key}/stats
  rejected_status: [401, 403, 404]
tools:
  exclude: [health, create_key]
```

`auth.fields` doubles as the form a host renders to ask for credentials.

| Manifest key | Meaning |
|---|---|
| `spec` | https URL or a path relative to the manifest |
| `base_url` | where requests and credentials go; required unless `auth.type` is `none` |
| `auth.type` | `bearer`, `header` (with `name`, optional `prefix`), `query` (with `name`), `basic`, `none` |
| `check` | request that validates credentials; `rejected_status` defaults to `[401, 403]` |
| `tools.include` / `exclude` | operation IDs (full or FastAPI-short) or tool names; an entry that matches nothing is an error |
| `tools.names` | rename the part after the slug: `{operationId: find_hotels}` gives `SLUG_FIND_HOTELS`; a full operation ID wins over a short one |
| `tools.allow_writes` | `false` keeps only GET and HEAD |

## What the library guarantees

- **Credentials go where the manifest says, once.** A pasted `Bearer ` prefix
  is not doubled. The document's own `Authorization` parameter, and any
  header the connector sets, never become model arguments.
- **Credentials never leave the app's host.** They go only to `base_url`;
  document paths that are not absolute paths are skipped, every request URL
  is checked against the base host, and redirects to another host are
  returned as results, not followed. No cookie jar is shared between people.
- **Arguments are checked before anything is sent**: unknown names, missing
  required values, `..` path segments and header line breaks are refused with
  an error that names the argument, so a model can correct the call.
- **Writes are sent once.** Failed requests are never retried automatically.
  `Tool.ReadOnly` is true only for GET and HEAD; a host that asks a person
  before a write should ask for every other tool.
- **App answers are results.** A 404 or 422 comes back as a `Result` with the
  status and body, which is what a model needs to see. Errors are reserved for
  calls that never got an answer, and they follow
  [go-kit `errs`](https://github.com/omniaura/go-kit/tree/main/errs): the
  message is safe to show a person, details stay server-side.
- **Bounded.** Successful responses are capped (256 KiB by default) and
  marked truncated; non-2xx bodies are kept up to 2 KiB; specs are capped at
  16 MiB and `$ref` expansion at 200,000 nodes per operation; requests time
  out after 30 s. Every success is reported as status 200.

Schemas come out as plain JSON Schema: local `$ref`s are expanded inline,
recursion is cut off, OpenAPI-only keys (`example`, `x-*`, `readOnly`
properties) are dropped and 3.0 `nullable` is folded into `type`. A JSON
object body with fixed properties is flattened into top-level arguments;
anything else is a single `body` argument. Operations the library cannot
call faithfully (form or multipart bodies, query styles other than `form`,
deprecated operations, schemas too large to expand) are listed
by `Skipped()` with the reason, never guessed at.

## agentflow

`adapters/agentflow` is a separate module, so the core stays free of model
SDKs:

```go
specs := afconnect.Specs(c)
catalog, _ := afagent.PrepareTools(ctx, declared, specs, allow)
out, err := catalog.Dispatch(ctx, call, declaredFn, afconnect.Dispatcher(c, credsFor))
```

## CLI

```sh
go run ./cmd/connectors list
go run ./cmd/connectors tools bluepillow
CONNECTOR_API_KEY=pk_anon_... go run ./cmd/connectors check bluepillow
CONNECTOR_API_KEY=pk_anon_... go run ./cmd/connectors call bluepillow BLUEPILLOW_RESOLVE_DESTINATION '{"name":"Paris"}'
```

Credentials are read from `CONNECTOR_<FIELD>` environment variables, never
from arguments.

## Tests

```sh
go test -race ./... ./adapters/agentflow/...
CONNECTORS_LIVE=1 CONNECTOR_API_KEY=pk_anon_... go test -run TestLiveBluePillow ./manifests/
```

Blue Pillow keys are anonymous and free:
`curl -X POST https://api.b2a.bluepillow.com/v1/keys -H 'content-type: application/json' -d '{"agent":"me"}'`.

## Not yet

OAuth 2, form and multipart bodies, remote `$ref`s, pagination helpers, and
response shaping. Contributions with a manifest and a test are welcome.

## License

MIT
