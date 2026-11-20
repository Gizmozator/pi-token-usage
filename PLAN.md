# pi-token-usage — Plan

Local web app that indexes `~/.pi/agent/sessions` into SQLite and charts token usage
per day / month / project / model. Go backend, Chart.js frontend, single binary.

---

## 1. What the session data actually looks like (verified on this machine)

`~/.pi/agent/sessions/` = **519 MB, 1,437 `.jsonl` files**. Every line is one JSON record.

### File taxonomy — this is the crux of correctness

| Pattern | Count | Meaning | Action |
|---|---|---|---|
| `<proj>/<ts>_<uuid>.jsonl` | 163 | Top-level session | **count** |
| `<proj>/<parent-stem>/forks/<ts>_<uuid>.jsonl` | 10 | Fork of a session | **count, but dedupe** (see below) |
| `<proj>/<parent-stem>/<child-uuid>/run-N/session.jsonl` | 935 | Subagent session | **count**, tag `kind=subagent` |
| `<proj>/subagent-artifacts/**/*_transcript.jsonl` | 329 | *Mirror* of subagent runs | **EXCLUDE** |

**Trap 1 — artifacts double-count.** Artifact transcripts use a different envelope
(`recordType`, not `type`) and carry **850,751,905 tokens** across 12,200 usage records
that are the *same* work already present in the nested `session.jsonl` files. Including
them would inflate totals by ~28%.

**Trap 2 — forks copy parent messages.** A fork file re-contains the parent's earlier
records with **identical message `id`s**. In the sampled pair: 12 shared ids, 5 carrying
usage. Across all fork files = **32,422,197 tokens**. Fork-*only* messages are genuinely
new work and must still count. → Dedupe on `(message_id, totalTokens)`.
(Note: message ids are 8 hex chars; ~1 accidental cross-session collision is expected at
96k messages, hence the token-count tiebreak.)

### Record types observed
`session` (header, has `cwd`) · `message` · `model_change` · `thinking_level_change`
· `session_info` (has `name`, e.g. `subagent-reviewer-<uuid>-1`) · `compaction`
· `custom` · `custom_message`

### Usage lives only on `message` records where `role == "assistant"`

```json
{"type":"message","id":"84ed2e6b","parentId":"...","timestamp":"2026-08-23T19:18:26.448Z",
 "message":{"role":"assistant","api":"openai-completions","provider":"Lemonade",
   "model":"Qwen3.8-27B-GGUF-UD-Q4_K_XL",
   "usage":{"input":2516,"output":274,"cacheRead":0,"cacheWrite":0,
            "reasoning":0,"totalTokens":2790,
            "cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}
```

Two shapes exist: 336 records omit `reasoning` → default 0.
Verified identity: `totalTokens == input + output + cacheRead + cacheWrite` on **all**
14,865 records (reasoning is *not* added into total here, but is tracked separately).

### Real totals from your data

| Metric | Value |
|---|---|
| Total tokens | **3,031,937,214** |
| cacheRead | 2,897,271,442 (**95.5%**) |
| input (fresh) | 107,203,518 |
| output | 27,462,254 |
| cacheWrite | 0 |
| reasoning | 0 |
| **cost** | **0 — all local models** |
| Providers | Lemonade 18,106 · llamacpp 16,432 · halogen 8,646 · vllm 3 · llama.cpp 2 |
| Months | 2026-07: 36M · 2026-08: 1.34B · 2026-09: 1.65B |
| Projects | ~16 |

**Design consequence:** cacheRead is 95.5% of everything. A naive stacked chart makes
`output` (27M) a 0.9% sliver — invisible. The UI must treat cache as a first-class
*separate* series with a toggle, not part of the same stack. Cache hit rate
`cacheRead/(cacheRead+input)` = **96.4%** is genuinely your most interesting metric.

**Cost is dropped entirely** from the default UI (all zeros). Optional "shadow price"
toggle instead — see §5.

---

## 2. Stack decision: **Go**

`go1.26.5` is installed (Python 3.14 also available, but no FastAPI).

Why Go wins here:
- **One static binary** — `go:embed` bundles the whole web UI. Ship `./pitokens`, no venv, no pip, no node_modules at runtime.
- **`modernc.org/sqlite`** — pure-Go SQLite driver, **no CGO**, cross-compiles trivially.
- 534 MB first parse ≈ a few seconds; concurrent via goroutines. Incremental re-scan is milliseconds.
- Native SSE + `fsnotify` for live mode.

Rejected: Python (needs venv + FastAPI install, worse distribution for a personal daemon).

---

## 3. Storage & incremental indexing

DB at `~/.local/share/pi-token-usage/index.db`. Never writes to `~/.pi`.

```sql
-- resume / skip logic
CREATE TABLE files (
  path       TEXT PRIMARY KEY,
  size       INTEGER NOT NULL,
  mtime_ns   INTEGER NOT NULL,
  offset     INTEGER NOT NULL DEFAULT 0,   -- bytes already parsed (append-only resume)
  indexed_at TEXT    NOT NULL
);

CREATE TABLE sessions (
  id                TEXT PRIMARY KEY,     -- from `session` header
  cwd               TEXT NOT NULL,
  project           TEXT NOT NULL,       -- basename(cwd), overridable
  kind              TEXT NOT NULL,       -- main | fork | subagent
  agent_name        TEXT,                -- from session_info: subagent-worker-… / -reviewer-…
  parent_session_id TEXT,                -- derived from nested path
  started_at        TEXT, ended_at TEXT,
  msgs              INTEGER,
  input INTEGER, output INTEGER, cache_read INTEGER, cache_write INTEGER,
  reasoning INTEGER, total INTEGER
);

-- raw counted facts, one row per counted assistant message (~43k rows)
-- This is the source of truth for all rollups.
CREATE TABLE usage_events (
  id          INTEGER PRIMARY KEY,
  ts_utc      TEXT NOT NULL,
  day         TEXT NOT NULL,      -- local-time bucket
  hour        INTEGER NOT NULL,   -- local hour 0-23 (see §10: keep grain rich)
  session_id  TEXT NOT NULL REFERENCES sessions(id),
  project     TEXT NOT NULL,
  model       TEXT NOT NULL,
  provider    TEXT NOT NULL,
  kind        TEXT NOT NULL,      -- main | fork | subagent
  input INTEGER, output INTEGER, cache_read INTEGER, cache_write INTEGER,
  reasoning INTEGER, total INTEGER
);
CREATE INDEX idx_ev_day ON usage_events(day);
CREATE INDEX idx_ev_proj ON usage_events(project);

-- the query cube: a MATERIALIZED VIEW rebuilt from usage_events, not incrementally updated
CREATE TABLE facts (
  day TEXT, hour INTEGER, project TEXT, model TEXT, provider TEXT, kind TEXT,
  input INTEGER, output INTEGER, cache_read INTEGER, cache_write INTEGER,
  reasoning INTEGER, total INTEGER, msgs INTEGER,
  PRIMARY KEY (day, hour, project, model, provider, kind)
);

-- fork-copy dedupe, survives incremental runs
CREATE TABLE seen_msg ( msg_id TEXT, tok INTEGER, PRIMARY KEY (msg_id, tok) );
```

**Why `facts` is rebuilt, not incrementally upserted.** After each index run, refresh it
with one statement:

```sql
BEGIN;
DELETE FROM facts;
INSERT INTO facts SELECT day,hour,project,model,provider,kind,
  sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(total),count(*)
  FROM usage_events GROUP BY 1,2,3,4,5,6;
COMMIT;
```

At ~43k event rows this takes milliseconds, and it makes the rollup **idempotent by
construction** — no double-counting when a file is re-parsed, and no drift. It also means
changing the timezone, the project-mapping rule, or the cube grain is a pure SQL rebuild
over `usage_events` with **no re-parse of the 519 MB of source files at all**.

**Scan algorithm**
1. Walk `**/*.jsonl`, **skip any path containing `subagent-artifacts`**.
2. Classify `kind` from path shape (table in §1).
3. If `size` and `mtime_ns` unchanged → skip file entirely.
4. If `size > offset` → seek to `offset`, parse only the new tail (append-only fast path).
5. If `size < offset` → file was rewritten/compacted → **full re-parse** of that file.
6. Per assistant message: `INSERT OR IGNORE INTO seen_msg` — if 0 rows inserted, it's a
   fork copy → don't add to aggregates.
7. Upsert `sessions` and `facts` in one transaction per file.

**Timezone:** store UTC, bucket `day` in **local time** (config `TZ`, default system).
Otherwise late-night work lands on the wrong day.

---

## 4. HTTP API

Binds `127.0.0.1:7777` only (privacy — this is personal usage data).

| Endpoint | Purpose |
|---|---|
| `GET /api/bootstrap` | **everything the UI needs in one shot** (cube + sessions + meta) — see §10 |
| `GET /api/overview` | totals, cache hit %, active days, projects, top model |
| `GET /api/series?gran=month\|week\|day&from&to&split=project\|model\|kind` | line charts (server-side variant, mostly unused) |
| `GET /api/heatmap?year=YYYY` | calendar cells `{date, tokens}` |
| `GET /api/projects` · `/api/models` · `/api/providers` | filter lists + totals |
| `GET /api/sessions?project=&kind=&limit=` | drill-down table |
| `POST /api/index` · `GET /api/index/status` | trigger / progress (files, bytes, ETA) |
| `GET /api/stream` | **SSE** — push new usage live as it's indexed (optional, off by default) |

## 5. UI — Chart.js, dark "oscilloscope" theme

Vendored `chart.umd.js` via `go:embed` → works fully offline.

1. **Hero row** — count-up animated numbers: total tokens, cache hit rate, active days, projects.
2. **Main monthly chart** — lines for **Input / Output / Total**, with `cacheRead` as a
   *dashed separate series* (off by default) plus a **log-scale switch**. This is the fix
   for the 95.5%-cache problem: you can see the real work and the cache on the same axes.
3. **Calendar heatmap** — hand-rolled canvas, GitHub-contribution style, intensity =
   `log(tokens)`. Instantly shows when you actually work.
4. **Cache efficiency radial gauge** — 96.4% today. The signature metric of local-model usage.
5. **Project small multiples** — one sparkline card per project, tokens/day, sortable.
6. **Model & provider share** — horizontal bars; donut for `kind` (main vs subagent).
7. **Subagent breakdown** — worker vs reviewer cost, since subagents are 935 of 1,108 sessions.
8. **Shadow price toggle** (optional, clearly labelled *hypothetical*) — you enter a
   reference `$/Mtok` for input/output/cacheRead; we show what the same 3.03B tokens
   *would* have cost on cloud. Since real cost is 0, this is the only way to get a $ figure,
   and it makes the local-vs-cloud case vividly.
9. **Live mode** — SSE stream, chart ticks as you keep coding.
10. Keyboard nav, `/` to filter projects.

---

## 6. Layout & milestones

```
pi-token-usage/
  go.mod
  cmd/pitokens/main.go          # CLI: index | serve | report
  internal/parser/              # jsonl -> typed events, tolerant of partial lines
  internal/index/               # walk, classify, resume, dedupe
  internal/store/               # sqlite schema + query funcs
  internal/httpapi/             # routes + SSE
  internal/web/                 # go:embed: index.html, app.js, style.css, vendor/chart.umd.js
  Makefile
```

| Milestone | Deliverable |
|---|---|
| **M1** | Parser + SQLite + `pitokens index` (incremental, dedupe, excludes artifacts) |
| **M2** | `pitokens report` — text tables per day/month/project (useful without UI) |
| **M3** | HTTP API + embedded Chart.js dashboard (hero, monthly lines, heatmap) |
| **M4** | Live SSE tail, shadow pricing, subagent breakdown, `systemd --user` unit |
| **M5** | Docker: multi-stage static image + `make docker-up` with correct mounts |

## 7. Gotchas checklist

- [ ] Exclude `subagent-artifacts/**` (else +850M phantom tokens)
- [ ] Dedupe fork-copied messages by `(msg_id, tok)` (else +32M)
- [ ] `reasoning` key absent on some records → default 0
- [ ] `size < offset` ⇒ rewritten file, full re-parse
- [ ] Last line may be mid-write while pi is running → tolerate JSON errors, don't advance offset
- [ ] Bucket days in local time
- [ ] `cost` is all-zero → never render cost as primary; use shadow pricing if needed
- [ ] Bind localhost only

---

## 8. Index refresh mechanism (how, not when — see §11 for the policy)

### Measured cost of each trigger (this machine)

| Operation | Cost |
|---|---|
| `stat()` pass over all 1,437 files | **4.1 ms** |
| Worst-case single file full parse (4.9 MB / 794 records) | **16 ms** |
| Cold full index of 534 MB | ~2 s (one time) |
| Idle file changes over 6 s of live pi | **0** |

Because a full staleness check costs 4 ms, **manual-only buys nothing** — and stale
numbers are worse than no numbers, because they get trusted.

### Write pattern (observed)

pi appends in **bursts per turn**, not continuously. This planning session's file went
9 KB → 100 KB across the conversation, then sat byte-identical during idle. So the
right primitive is *event-driven with debounce*, not polling.

### Trigger design (initial proposal — **revised by §11** given the relaxed freshness target)

The mechanism below still holds; what changed is that the `inotify` watcher and SSE tail
are dropped, leaving refresh-on-page-load as the only required path.

1. **Lazy refresh on page load** — the important one. Stat pass (4 ms) → index only
   changed files → render. Gives correct numbers with *zero* background process, and
   means a one-shot `pitokens serve` is already correct.
2. **Background watcher while serving** — `inotify` recursive on the sessions root,
   **~2 s debounce coalesced per file**. Near-zero idle cost (0 events when idle).
   Drives the SSE live tail.
3. **Manual override, always visible** — "Re-index now" button + `pitokens index` /
   `pitokens index --full`. Non-negotiable escape hatch for: TZ or project-mapping rule
   changes (require full re-derivation), compaction-rewritten files, and debugging.

### Trigger rule: **size change, not mtime**

Re-parse only when `size != stored_size`. `mtime`-only changes are a trap — rsync,
restores, `touch` and backup tools churn mtimes with no new content, which would trigger
pointless re-parses of the whole 519 MB tree. If size is unchanged there is nothing new
readable, so skip.

| Condition | Action |
|---|---|
| `size == stored` | skip |
| `size > stored` | tail-parse from `offset` (fast path) |
| `size < stored` | rewritten/compacted → **full re-parse** that file |
| new file | full parse |

### Guardrails

- **Single-flight lock** — never two indexers concurrently; coalesce queued requests and
  expose progress (`files done / total`, bytes, ETA) via `/api/index/status`.
- **Never auto-rerun the cold index.** Only `--full` may clear the `offset` table.
- **Today is always partial.** Render a "live" pulse on today's bar rather than letting a
  mid-day number look final — otherwise the daily chart appears to dip every morning.
- **Tolerate torn writes**: pi may append mid-parse; on a JSON error stop at that line and
  do *not* advance `offset` past it.
- `PITOKENS_WATCH=0` disables the watcher for cron/CI one-shot use.

---

## 9. Docker support (local only)

### Image: multi-stage, static, `scratch`-final

`CGO_ENABLED=0` + `modernc.org/sqlite` means the binary is fully static — no libc, so a
`FROM scratch` final stage works and lands at **~22 MB**.

```dockerfile
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/pitokens ./cmd/pitokens

FROM scratch
COPY --from=build /out/pitokens /pitokens
COPY --from=build --chown=65534:65534 /out/pitokens /dev/null  # placeholder; see note
USER 65534:65534
EXPOSE 7777
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s \
  CMD ["/pitokens", "healthcheck"]
ENTRYPOINT ["/pitokens", "serve"]
```

**Two scratch-specific tricks:**

- **`import _ "time/tzdata"` in `main.go`.** `scratch` has no `/usr/share/zoneinfo`, so
  `TZ=Europe/Helsinki` would silently fail and **bucket every day in UTC** — a 3-hour
  shift that quietly corrupts all daily/monthly numbers in summer (EEST). Embedding tzdata
  in the binary makes `TZ` work with no mount. This is the single most important Docker
  detail for this app.
- **`HEALTHCHECK` needs no curl** — implement a `pitokens healthcheck` subcommand that
  dials the local listener and exits 0/1.

### Mounts (the part that must be right)

| Host | Container | Mode | Why |
|---|---|---|---|
| `~/.pi/agent/sessions` | `/sessions` | **`:ro`** | pi's live data; the app must never write here |
| named volume `pitokens-data` | `/data` | rw | SQLite DB; survives rebuilds, avoids host UID/GID friction |

```bash
docker run -d --name pitokens \
  -p 127.0.0.1:7777:7777 \
  -v "$HOME/.pi/agent/sessions:/sessions:ro" \
  -v pitokens-data:/data \
  -e TZ=Europe/Helsinki \
  -e PI_SESSIONS_DIR=/sessions \
  -e PITOKENS_DB=/data/index.db \
  -e PITOKENS_ADDR=0.0.0.0:7777 \
  --read-only --tmpfs /tmp \
  pi-token-usage:latest
```

Notes on each choice:

- **`-p 127.0.0.1:7777:7777`** — never bare `-p 7777:7777`; that publishes your personal
  usage history to the whole LAN. Inside the container bind to `0.0.0.0` so the published
  port works; the loopback restriction lives on the host side.
- **Named volume over bind mount for `/data`** — avoids the host-UID/permission dance and
  keeps `~/.local/share` clean. Use a bind mount only if you want the DB file directly
  browsable.
- **SELinux is `Enforcing` on this Fedora 44 host.** Any *bind* mount of a home-directory
  path needs a relabel suffix — `:z` (private) or `:Z` (shared) — or you get
  `Permission denied` on a path that looks perfectly correct. Named volumes dodge this.
- **`--read-only --tmpfs /tmp`** — hardening; the app only ever writes the DB.

### Makefile targets

```makefile
IMAGE    ?= pi-token-usage:latest
SESSIONS ?= $(HOME)/.pi/agent/sessions
DB_VOL   ?= pitokens-data
TZ       ?= Europe/Helsinki
PORT     ?= 7777

docker-build:            ## build the static image
	docker build -t $(IMAGE) .

docker-up: docker-build  ## start (auto cold-index on first run, then watch)
	docker rm -f pitokens 2>/dev/null || true
	docker run -d --name pitokens \
	  -p 127.0.0.1:$(PORT):7777 \
	  -v "$(SESSIONS):/sessions:ro" \
	  -v $(DB_VOL):/data \
	  -e TZ=$(TZ) -e PI_SESSIONS_DIR=/sessions \
	  -e PITOKENS_DB=/data/index.db -e PITOKENS_ADDR=0.0.0.0:7777 \
	  --read-only --tmpfs /tmp \
	  $(IMAGE)
	@echo "→ http://127.0.0.1:$(PORT)"

docker-down:   ; docker stop pitokens
docker-logs:   ; docker logs -f pitokens
docker-reindex:; docker exec pitokens /pitokens index        ## incremental
docker-rebuild-index:                                       ## full, clears offsets
	docker exec pitokens /pitokens index --full
docker-clean:  ; docker rm -f pitokens; docker volume rm $(DB_VOL)
docker-shell:  ; docker run --rm -it --entrypoint /pitokens $(IMAGE) report
```

First `make docker-up` cold-indexes in ~2 s then switches to watch mode — no manual step.

### Docker gotchas

- [ ] **`TZ` must be set explicitly** and tzdata embedded, or every day bucket is UTC
- [ ] Sessions mount is **read-only** — verify the app never writes to `/sessions`
- [ ] Loopback-only port publish
- [ ] SELinux `:z`/`:Z` suffix if switching `/data` to a bind mount
- [ ] Cold index on empty volume must not block healthcheck → `--start-period=15s`
- [ ] `--read-only` rootfs: any temp writes go to the `/tmp` tmpfs

---

## 10. Client data strategy: **load once, filter in JS**

**Answer: no.** Changing the project / model / date filter must **not** trigger a page
load, a re-index, or any server round-trip. Measured payload sizes make this obvious:

| Dataset | Rows | JSON | gzipped |
|---|---|---|---|
| **Cube** `(day, project, model, provider, kind)` | **112** | 9.5 KB | **1.3 KB** |
| Sessions (drill-down list) | 949 | 102.5 KB | 23.1 KB |
| Raw events (per assistant message) | 43,218 | 3,160 KB | 362.5 KB |

The whole multi-dimensional dataset is **1.3 KB gzipped**. A project filter is a
`.filter()` over 112 objects — sub-millisecond, and far cheaper than an HTTP round-trip
even on loopback. Shipping the 43k raw events would be 280× larger for no benefit.

### Flow

```
page load ──> GET /api/bootstrap  (cube + sessions + meta, ~25 KB gz)
              └─ server does the 4 ms staleness check + indexes changed files first
all filters ──> pure client-side reduce over cube. ZERO network.
re-index    ──> only on page load, or the explicit "Re-index now" button.
```

**The key decoupling:** *index freshness* and *view state* are independent. Filters never
touch the index; the index never resets your filters.

### Design consequences

- **Grain must be chosen up front**, because the cube can only answer questions at the
  grain it stores. Current grain covers day/project/model/provider/kind. If you later want
  the hour-of-day circadian chart, add `hour` to the grain now — that's 24× rows
  (~2,700, still ~15 KB gz) and avoids a re-parse of 519 MB later.
- **Growth is a non-issue.** ~1.5 projects/day observed, so 5 years of history ≈ 1,500–3,000
  cube rows ≈ ~20 KB gzipped. This architecture stays instant for years with no server-side
  query layer.
- **URL-encoded filter state** (`#project=foo&gran=month&from=2026-08`) so views are
  bookmarkable/shareable — still zero server involvement.
- **Chart.js**: keep one chart instance and swap `chart.data` + `chart.update('none')`.
  Never destroy/recreate per filter change.
- **`/api/bootstrap` reads from SQLite**, so historical days keep rendering even if pi
  later prunes the underlying `.jsonl` files — the DB is the durable record.
- Raw events stay server-side; fetch a page of them only if a per-message drill-down is
  ever built.

---

## 11. Freshness target: hours is fine → drop the watcher

Given "a few hours of staleness is acceptable" and "fast is fine if it's cheap":

**Page-load refresh satisfies both, and is strictly better than a few hours** — it costs
4 ms and returns fully current data. So the `inotify` watcher and SSE tail are complexity
with no benefit. Recommendation:

| Mechanism | Verdict |
|---|---|
| Refresh-on-page-load (stat pass + parse changed files) | **keep — the default and only required path** |
| `inotify` recursive watcher | **drop** (keep behind `PITOKENS_WATCH=1` if ever wanted) |
| SSE live tail | **drop from M4** (was solving a problem you don't have) |
| Optional poll while tab focused | 5 min max, `document.visibilityState` gated — optional nicety |

The incremental design already gives you the past-history behaviour for free: historical
files never change size, so the 4 ms stat pass skips all ~1,100 of them and only the
current day's file is ever re-read.

### Re-scope the UI toward past history

Since previous days are the interest, the default view should **not** be today:

- **Landing view = all-time / last-3-months monthly lines**, not a live dashboard.
- Today demoted to a small footnote, labelled *partial*.
- Add: month-over-month deltas, cumulative running-total curve, weekday-average profile,
  hour-of-day heatmap (needs `hour` in cube grain), per-project lifetime totals,
  "where did my tokens go" treemap-style breakdown.
- Range picker with an **All time** option, since history is the point.

---

## Changelog

### 2026-11-21 — web UI restyled with the rantaeskola.fi design system

- Palette remapped to the house design system: navy base (#0b0f19), cyan
  primary (#38bdf8), indigo secondary (#818cf8), emerald/amber/rose accents;
  chart series, heatmap gradient, bar gradients and hero gradient re-tinted
  to match.
- `< host />` mono logo (cyan angle brackets) replaces the checker-square
  mark; the brand and the browser tab title derive from the host the page is
  served on (fallback "pi-tokens" on dev hosts) — no hardcoded domain.
