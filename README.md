# pi-token-usage

Local dashboard for token usage by the [pi](https://pi.dev) coding agent. Parses
`~/.pi/agent/sessions` into SQLite and charts it per day / month / project / model.

Single static Go binary, Chart.js UI, no external services. All data stays on your
machine and the server binds to loopback only.

## Quick start

```bash
make build
make index          # parse sessions into ~/.local/share/pi-token-usage/index.db
make run            # http://127.0.0.1:7777
```

Or without the browser:

```bash
make report BY=project
make report BY=month
```

## Docker

```bash
make docker-up      # http://127.0.0.1:7777
make docker-logs
make docker-down
```

The image is a 16 MB `scratch` container. `make docker-up` mounts everything
correctly:

| Host | Container | Mode |
|---|---|---|
| `~/.pi/agent/sessions` | `/sessions` | read-only |
| named volume `pitokens-data` | `/data` | read-write |

Override anything:

```bash
make docker-up SESSIONS=/other/sessions PORT=8080 TZ=Europe/Berlin DB_VOL=my-vol
```

Other targets: `docker-reindex`, `docker-rebuild-index`, `docker-status`,
`docker-shell CMD="report -by model"`, `docker-clean` (also deletes the database).

## How it works

```
~/.pi/agent/sessions/**/*.jsonl
        │  walk, classify, skip mirrors
        ▼
   parser        exact byte-offset resume, dedupe fork copies
        ▼
   SQLite      usage_events  ──GROUP BY──▶  facts (rollup)
        ▼
   HTTP API    /api/bootstrap  (whole cube, ~50 KB gzipped)
        ▼
   browser     every filter is client-side; no request per filter change
```

### Incremental by design

A full staleness check over ~1100 files costs about **4 ms**, so the index refreshes
on page load rather than running a background watcher. The trigger is **file size,
not mtime** — rsync, restores and `touch` churn mtimes without adding content, and
keying on mtime would force pointless re-parses of hundreds of megabytes.

| Condition | Action |
|---|---|
| size unchanged | skip |
| size grew | tail-parse from the stored offset |
| size shrank | rewritten/compacted → full re-parse of that file |

Cold index of 519 MB takes ~3 s. Incremental re-runs take milliseconds.

## Things that will bite you (already handled here)

These are the traps in the session data. If you fork this design, keep them.

1. **`subagent-artifacts/**` is a mirror.** It uses a different envelope
   (`recordType`, not `type`) and duplicates subagent runs that already exist as
   nested `session.jsonl` files. Counting it inflates totals by ~28%.

2. **Fork files copy parent records byte-identically**, including message ids.
   Dedupe on `(message_id, totalTokens)` scoped against a *different* source file —
   scoped too broadly it silently deletes a file's own events when it is re-parsed.

3. **`bufio.ReadLine()` strips the newline.** Counting only the returned chunk
   length drifts the resume offset by one byte per line, so the next pass starts
   mid-line, every record fails to parse, and that file silently stops being
   indexed. Offsets must include terminators.

4. **`cacheRead` is ~95% of all tokens.** A naive stacked chart makes `output` an
   invisible sliver. It needs to be a separate series, and cache hit rate is the
   genuinely interesting metric.

5. **`TZ` must be embedded and set.** A `scratch` image has no `/usr/share/zoneinfo`,
   so `TZ=Europe/Helsinki` silently fails and every day bucket lands in UTC — a
   multi-hour shift that quietly corrupts daily and monthly numbers. This project
   imports `time/tzdata`.

6. **Named volumes mount their root as `root:root`.** Docker copies the *contents*
   of the image path but not the directory's own ownership, so a non-root container
   mounted at `/data` cannot create its database. The DB lives in `/data/db`,
   which ships in the image with ownership preserved.

7. **A torn trailing write is normal** — pi appends while you read. Never commit an
   offset past an incomplete line; stop before it and re-read next pass.

## CLI

```
pitokens index [--full]     parse sessions into the database
pitokens serve              serve UI + API (cold-indexes first)
pitokens report -by X       text tables: day|month|project|model|provider|kind
pitokens healthcheck        exit 0 if the listener answers
```

Environment:

| Variable | Default | Purpose |
|---|---|---|
| `PI_SESSIONS_DIR` | `~/.pi/agent/sessions` | sessions root |
| `PITOKENS_DB` | `~/.local/share/pi-token-usage/index.db` | database path |
| `PITOKENS_ADDR` | `127.0.0.1:7777` | listen address |
| `TZ` | system local zone | day/hour bucketing |

## API

| Endpoint | Purpose |
|---|---|
| `GET /api/bootstrap` | cube + sessions + facets + totals in one call |
| `GET /api/totals` | headline numbers |
| `GET /api/cube` | raw rollup |
| `GET /api/sessions?limit=N` | session drill-down |
| `GET /api/status` | index freshness and table sizes |
| `POST /api/index?full=true` | trigger a re-index |

Responses are gzipped when requested.

## Development

```bash
make test    # unit tests (clean under -race)
make vet
```

The test suite deliberately covers the failure modes above rather than the happy
path — offset drift, torn writes, self-dedupe, rollup idempotency, and late-night
day bucketing.

## Notes on cost

`cost` is zero throughout the session data because the models run locally. The
dashboard therefore reports tokens, not money.
