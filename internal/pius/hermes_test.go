package pius

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// createHermesDB builds a minimal stand-in for hermes-agent's state.db: the
// sessions and session_model_usage tables (and columns) the indexer reads.
// session_model_usage counters are cumulative and updated in place.
func createHermesDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			parent_session_id TEXT,
			started_at REAL NOT NULL,
			cwd TEXT
		);
		CREATE TABLE session_model_usage (
			session_id TEXT NOT NULL,
			model TEXT NOT NULL,
			billing_provider TEXT NOT NULL DEFAULT '',
			billing_base_url TEXT NOT NULL DEFAULT '',
			billing_mode TEXT NOT NULL DEFAULT '',
			task TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			first_seen REAL,
			last_seen REAL,
			PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task)
		);`); err != nil {
		t.Fatal(err)
	}
	return p
}

func hermesUpsertUsage(t *testing.T, db *sql.DB, session, model, provider, task string,
	in, out, cacheR, cacheW, reasoning int, lastSeen float64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO session_model_usage
		(session_id, model, billing_provider, task, input_tokens, output_tokens,
		 cache_read_tokens, cache_write_tokens, reasoning_tokens, last_seen)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id, model, billing_provider, billing_base_url, billing_mode, task)
		DO UPDATE SET input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			cache_read_tokens=excluded.cache_read_tokens, cache_write_tokens=excluded.cache_write_tokens,
			reasoning_tokens=excluded.reasoning_tokens, last_seen=excluded.last_seen`,
		session, model, provider, task, in, out, cacheR, cacheW, reasoning, lastSeen)
	if err != nil {
		t.Fatal(err)
	}
}

func seedHermes(t *testing.T) string {
	t.Helper()
	p := createHermesDB(t)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	started := float64(1793649492) // arbitrary unix seconds
	if _, err := db.Exec(`INSERT INTO sessions (id, source, parent_session_id, started_at, cwd) VALUES
		('local-aaa', 'local', NULL, ?, '/home/u/projA'),
		('local-bbb', 'local', 'local-aaa', ?, '/home/u/projB')`, started, started+10); err != nil {
		t.Fatal(err)
	}
	hermesUpsertUsage(t, db, "local-aaa", "model-x", "custom", "", 100, 40, 500, 0, 10, started+100)
	hermesUpsertUsage(t, db, "local-aaa", "model-x", "custom", "title_generation", 20, 5, 0, 0, 0, started+200)
	hermesUpsertUsage(t, db, "local-bbb", "model-y", "", "", 7, 3, 0, 0, 0, started+300)
	return p
}

func TestHermesIndexBasic(t *testing.T) {
	p := seedHermes(t)
	s := newTestStore(t)

	res := &IndexResult{}
	if err := IndexHermes(s, p, res, false); err != nil {
		t.Fatal(err)
	}
	if res.EventsAdded != 3 {
		t.Fatalf("events added=%d want 3", res.EventsAdded)
	}

	var src, project, kind, model, provider string
	var total int
	err := s.db.QueryRow(`SELECT source, project, kind, model, provider, total
		FROM usage_events WHERE session_id='local-aaa' AND msg_id NOT LIKE '%title_generation%'`).
		Scan(&src, &project, &kind, &model, &provider, &total)
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceHermes || project != "projA" || kind != string(KindMain) || model != "model-x" || provider != "custom" {
		t.Fatalf("main row: src=%s project=%s kind=%s model=%s provider=%s", src, project, kind, model, provider)
	}
	if total != 650 {
		t.Fatalf("main row total=%d want 650", total)
	}

	// Subagent session (parent_session_id set) and empty provider fallback.
	err = s.db.QueryRow(`SELECT kind, provider FROM usage_events WHERE session_id='local-bbb'`).
		Scan(&kind, &provider)
	if err != nil {
		t.Fatal(err)
	}
	if kind != string(KindSubagent) || provider != "hermes" {
		t.Fatalf("subagent row: kind=%s provider=%s want subagent/hermes", kind, provider)
	}

	var sessSrc, agent string
	if err := s.db.QueryRow(`SELECT source, agent FROM sessions WHERE id='local-aaa'`).Scan(&sessSrc, &agent); err != nil {
		t.Fatal(err)
	}
	if sessSrc != SourceHermes || agent != "hermes" {
		t.Fatalf("session meta: source=%s agent=%s", sessSrc, agent)
	}
}

// The cumulative counters grow in place; each index run must insert only the
// growth, never the whole cumulative value again.
func TestHermesDeltaIngest(t *testing.T) {
	p := seedHermes(t)
	s := newTestStore(t)
	if err := IndexHermes(s, p, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	// The live session's counters grow: +50 input, +20 output, +1000 cache read.
	hermesUpsertUsage(t, db, "local-aaa", "model-x", "custom", "", 150, 60, 1500, 0, 10, 1793649900)
	// A shrink (rewind): baseline resets, no negative event.
	hermesUpsertUsage(t, db, "local-bbb", "model-y", "", "", 2, 1, 0, 0, 0, 1793649950)
	db.Close()

	res := &IndexResult{}
	if err := IndexHermes(s, p, res, false); err != nil {
		t.Fatal(err)
	}
	if res.EventsAdded != 1 {
		t.Fatalf("growth events=%d want 1 (shrink emits nothing)", res.EventsAdded)
	}

	var in, out, cacheR, total int
	err = s.db.QueryRow(`SELECT input, output, cache_read, total FROM usage_events
		WHERE session_id='local-aaa' AND msg_id NOT LIKE '%title_generation%'
		ORDER BY id DESC LIMIT 1`).Scan(&in, &out, &cacheR, &total)
	if err != nil {
		t.Fatal(err)
	}
	if in != 50 || out != 20 || cacheR != 1000 || total != 1070 {
		t.Fatalf("delta event: in=%d out=%d cache=%d total=%d want 50/20/1000/1070", in, out, cacheR, total)
	}

	// Nothing changed: the run must be a no-op.
	res2 := &IndexResult{}
	if err := IndexHermes(s, p, res2, false); err != nil {
		t.Fatal(err)
	}
	if res2.EventsAdded != 0 {
		t.Fatalf("no-op run added=%d want 0", res2.EventsAdded)
	}
}

// A full re-ingest must re-derive the same totals from the cumulative state,
// not double-count and not lose the growth already folded in.
func TestHermesFullReingest(t *testing.T) {
	p := seedHermes(t)
	s := newTestStore(t)
	if err := IndexHermes(s, p, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}
	if err := IndexHermes(s, p, &IndexResult{}, true); err != nil {
		t.Fatal(err)
	}
	var n int
	var sum int64
	s.db.QueryRow(`SELECT count(*), sum(total) FROM usage_events WHERE source='hermes'`).Scan(&n, &sum)
	// 650 + 25 + 10 = 685 across 3 keys.
	if n != 3 || sum != 685 {
		t.Fatalf("after full re-ingest: events=%d sum=%d want 3/685", n, sum)
	}
}

func TestHermesMissingDBIsNoop(t *testing.T) {
	s := newTestStore(t)
	missing := filepath.Join(t.TempDir(), "nope.db")
	if err := IndexHermes(s, missing, &IndexResult{}, false); err != nil {
		t.Fatalf("missing db must be a no-op, got: %v", err)
	}
	if err := IndexHermes(s, "", &IndexResult{}, false); err != nil {
		t.Fatalf("empty path must be a no-op, got: %v", err)
	}
}

func TestHermesCubeCarriesSource(t *testing.T) {
	p := seedHermes(t)
	s := newTestStore(t)
	if err := IndexHermes(s, p, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RebuildFacts(); err != nil {
		t.Fatal(err)
	}
	cube, err := s.Cube()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range cube {
		if r.Source == SourceHermes {
			found = true
		}
	}
	if !found {
		t.Fatalf("cube missing hermes source: %+v", cube)
	}
}
