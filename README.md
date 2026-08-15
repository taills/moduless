# moduless

**English** · [简体中文](README.zh-CN.md) · [繁體中文](README.zh-TW.md)

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A Go web gateway whose features are plugins — separate processes that Core
starts, supervises, hot-reloads and upgrades without dropping a request.

Plugins serve their own HTTP APIs, ship their own micro-frontends, and can
intercept any request in the gateway's lifecycle, in the style of an IIS
filter. Enabling one makes its menu appear in the console; disabling one makes
it disappear, with no page reload.

```
Browser ──HTTP──▶ Core (:80)
                    ├─ filter pipeline   pre_route → authenticate → authorize →
                    │                    pre_handler → [backend] → post_handler → log
                    ├─ /api/plugins/*    a plugin's own HTTP API
                    ├─ /plugins/*        a plugin's micro-frontend
                    └─ PluginHost ──exec──▶ plugin subprocess ×N
                         │  HashiCorp go-plugin over a unix socket
                         ▼
                       HostServices: documents · durable queue · cache · locks
                       config · files · outbound HTTP · events · logs & metrics
```

Core listens on one port. Plugins open none.

## Why subprocesses

Core starts each plugin with `exec`, which is what makes the rest possible:

- **Hot load, unload and upgrade.** Core owns the process, so it can start a
  new version, health-check it, swap traffic atomically and drain the old one.
  Measured with continuous traffic across a swap: zero failed requests.
- **Crash isolation with recovery.** A plugin panic does not touch Core. The
  supervisor restarts it with exponential backoff and quarantines one that
  keeps crashing.
- **No network exposure.** There is no port to reach a plugin on, and no
  registration protocol to authenticate — Core is the parent process.

## Filters

A plugin declares which lifecycle phases and paths it cares about, and Core
compiles that into a match table. Requests nobody subscribed to cost almost
nothing:

| | |
|---|---|
| phase with no subscribers | **1.9 ns**, zero allocations |
| subscribed, path does not match | **8.2 ns**, zero allocations |
| an actual cross-process filter call | ~37,000 ns |

Filters default to fail-open, because most of them observe rather than guard
and a broken observer should not take the site down. Anything enforcing a
security decision opts in to `fail_closed`.

## A plugin

```go
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /notes", listNotes)

	sdk.Serve(sdk.Config{
		Handler: mux,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhasePreRoute: rateLimit,
		},
		Jobs: map[string]sdk.JobFunc{
			"nightly-summary": summarise,
		},
	})
}
```

`sdk.Serve` takes a standard `http.Handler`, so any router or middleware works
unchanged. Everything a plugin can reach — the document store, the queue,
caching, locks, files, outbound HTTP — goes through Core, and every call
carries the request's trace id automatically, so a slow query is attributable
to the request that caused it.

Full guide: [docs/plugin-development.md](docs/plugin-development.md).
Working example: [`extension-example/notes`](extension-example/notes).

## Running

```bash
git clone git@github.com:taills/moduless.git
cd moduless

# Build the console once
cd core/frontend && npm install && npm run build && cd ../..

# Build the example plugin into the plugin directory
mkdir -p plugins/notes/bin
CGO_ENABLED=0 go build -o plugins/notes/bin/plugin ./extension-example/notes
cp extension-example/notes/manifest.yaml plugins/notes/

# Run. Without DATABASE_URL the data, queue and file capabilities report
# Unavailable and everything else still works.
PLUGIN_DIR=./plugins go run ./core
```

Or with Docker:

```bash
docker compose up --build   # console at http://localhost:8080, admin / admin123
```

## Data

Plugins never connect to PostgreSQL. They declare collections in
`manifest.yaml`, Core provisions the tables, and access goes through a
document store with sorting, keyset pagination, aggregation, batch writes,
transactions and optimistic locking.

Keyset pagination rather than OFFSET is deliberate: OFFSET makes the database
walk and discard every skipped row, so deep pages get slower, and rows shifting
between requests silently duplicate or skip entries.

The durable queue is PostgreSQL-backed — at-least-once delivery with retries,
backoff, dead-lettering, delayed messages and deduplication, without adding a
broker to the deployment.

## Trust model

Plugins are reviewed by an operator before installation and run with Core's own
privileges, like an ISAPI filter inside the IIS worker process. Safety comes
from only installing plugins you trust.

Core does enforce, on its own side of the connection: the permission set a
plugin declared, per-plugin namespacing of documents, cache, queue and files,
transaction ownership, the outbound HTTP allow-list (including refusing
addresses that resolve to private or link-local ranges), and a SHA-256 check
that the binary is the one that was installed.

It does not confine the filesystem, CPU or system calls. Genuinely untrusted
code belongs behind a container boundary.

## Testing

```bash
go test ./... -race

# Database-backed tests skip without this
TEST_DATABASE_URL='postgres://postgres:pass@localhost:5432/test?sslmode=disable' go test ./...
```

The end-to-end suite forks real plugin processes and drives them over real
HTTP, including a hot upgrade under continuous load and a deliberate crash.

## License

Apache 2.0 — see [`LICENSE`](LICENSE). Dependency licence review in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
