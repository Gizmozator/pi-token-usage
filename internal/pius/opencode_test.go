package pius

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// createOpenCodeDB builds a minimal stand-in for opencode's SQLite database:
// just the session/message tables (and columns) the indexer reads.
func createOpenCodeDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE session (
			id text PRIMARY KEY,
			parent_id text,
			directory text NOT NULL,
			agent text,
			time_created integer NOT NULL
		);
		CREATE TABLE message (
			id text PRIMARY KEY,
			session_id text NOT NULL,
			data text NOT NULL
		);`); err != nil {
		t.Fatal(err)
	}
	return p
}

func ocInsertSession(t *testing.T, db *sql.DB, id, parent, dir, agent string, createdMS int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO session (id, parent_id, directory, agent, time_created) VALUES (?,?,?,?,?)`,
		id, parent, dir, agent, createdMS)
	if err != nil {
		t.Fatal(err)
	}
}

func ocInsertMsg(t *testing.T, db *sql.DB, id, sessionID, role, model, provider string, completedMS int64, in, out, cacheR, total int) {
	t.Helper()
	data := map[string]any{
		"role":       role,
		"modelID":    model,
		"providerID": provider,
		"agent":      "build",
		"time":       map[string]any{"created": completedMS - 1000, "completed": completedMS},
		"tokens": map[string]any{
			"total":  total,
			"input":  in,
			"output": out,
			"cache":  map[string]any{"read": cacheR, "write": 0},
		},
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message (id, session_id, data) VALUES (?,?,?)`, id, sessionID, string(b)); err != nil {
		t.Fatal(err)
	}
}

func seedOpenCode(t *testing.T) string {
	t.Helper()
	p := createOpenCodeDB(t)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC).UnixMilli()
	ocInsertSession(t, db, "ses_1", "", "/home/u/projA", "build", ts)
	ocInsertSession(t, db, "ses_2", "ses_1", "/home/u/projB", "task", ts)
	completed := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC).UnixMilli()
	ocInsertMsg(t, db, "msg_1", "ses_1", "assistant", "model-a", "prov-x", completed, 100, 40, 500, 640)
	ocInsertMsg(t, db, "msg_2", "ses_1", "user", "", "", completed, 0, 0, 0, 0)
	ocInsertMsg(t, db, "msg_3", "ses_2", "assistant", "model-b", "prov-y", completed+5000, 5, 7, 0, 12)
	ocInsertMsg(t, db, "msg_4", "ses_2", "assistant", "model-b", "prov-y", completed+6000, 0, 0, 0, 0) // zero tokens: not counted
	return p
}

func TestOpenCodeIndexBasic(t *testing.T) {
	ocPath := seedOpenCode(t)
	s := newTestStore(t)

	res := &IndexResult{}
	if err := IndexOpenCode(s, ocPath, res, false); err != nil {
		t.Fatal(err)
	}
	if res.EventsAdded != 2 {
		t.Fatalf("events added=%d want 2 (user and zero-token messages excluded)", res.EventsAdded)
	}

	var src, project, kind, model, day string
	var hour, total int
	err := s.db.QueryRow(`SELECT source, project, kind, model, day, hour, total
		FROM usage_events WHERE msg_id='msg_1'`).
		Scan(&src, &project, &kind, &model, &day, &hour, &total)
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceOpenCode || project != "projA" || kind != string(KindMain) || model != "model-a" {
		t.Fatalf("msg_1: src=%s project=%s kind=%s model=%s", src, project, kind, model)
	}
	if day != "2026-01-02" || hour != 10 || total != 640 {
		t.Fatalf("msg_1 bucketing: day=%s hour=%d total=%d", day, hour, total)
	}

	err = s.db.QueryRow(`SELECT project, kind FROM usage_events WHERE msg_id='msg_3'`).
		Scan(&project, &kind)
	if err != nil {
		t.Fatal(err)
	}
	if project != "projB" || kind != string(KindSubagent) {
		t.Fatalf("child session: project=%s kind=%s want projB/subagent", project, kind)
	}

	var sessSrc, agent string
	if err := s.db.QueryRow(`SELECT source, agent FROM sessions WHERE id='ses_1'`).Scan(&sessSrc, &agent); err != nil {
		t.Fatal(err)
	}
	if sessSrc != SourceOpenCode || agent != "build" {
		t.Fatalf("session meta: source=%s agent=%s", sessSrc, agent)
	}
}

func TestOpenCodeIncrementalTail(t *testing.T) {
	ocPath := seedOpenCode(t)
	s := newTestStore(t)

	if err := IndexOpenCode(s, ocPath, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}

	// New message appears in the opencode db.
	db, err := sql.Open("sqlite", ocPath)
	if err != nil {
		t.Fatal(err)
	}
	ocInsertMsg(t, db, "msg_5", "ses_1", "assistant", "model-a", "prov-x",
		time.Date(2026, 1, 3, 8, 0, 0, 0, time.UTC).UnixMilli(), 9, 1, 0, 10)
	db.Close()

	res := &IndexResult{}
	if err := IndexOpenCode(s, ocPath, res, false); err != nil {
		t.Fatal(err)
	}
	if res.EventsAdded != 1 {
		t.Fatalf("tail added=%d want 1", res.EventsAdded)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&n)
	if n != 3 {
		t.Fatalf("total events=%d want 3", n)
	}

	// Nothing new: the run must skip without touching events.
	res2 := &IndexResult{}
	if err := IndexOpenCode(s, ocPath, res2, false); err != nil {
		t.Fatal(err)
	}
	if res2.EventsAdded != 0 || res2.FilesSkipped != 1 {
		t.Fatalf("no-op run: added=%d skipped=%d want 0/1", res2.EventsAdded, res2.FilesSkipped)
	}
}

func TestOpenCodeFullReingestDoesNotDoubleCount(t *testing.T) {
	ocPath := seedOpenCode(t)
	s := newTestStore(t)

	if err := IndexOpenCode(s, ocPath, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}
	if err := IndexOpenCode(s, ocPath, &IndexResult{}, true); err != nil {
		t.Fatal(err)
	}
	var n int
	var sum int64
	s.db.QueryRow(`SELECT count(*), sum(total) FROM usage_events WHERE source='opencode'`).Scan(&n, &sum)
	if n != 2 || sum != 652 {
		t.Fatalf("after full re-ingest: events=%d sum=%d want 2/652", n, sum)
	}
}

func TestOpenCodeMissingDBIsNoop(t *testing.T) {
	s := newTestStore(t)
	missing := filepath.Join(t.TempDir(), "nope.db")
	if err := IndexOpenCode(s, missing, &IndexResult{}, false); err != nil {
		t.Fatalf("missing db must be a no-op, got: %v", err)
	}
	if err := IndexOpenCode(s, "", &IndexResult{}, false); err != nil {
		t.Fatalf("empty path must be a no-op, got: %v", err)
	}
}

func TestOpenCodeCubeAndFacetsCarrySource(t *testing.T) {
	ocPath := seedOpenCode(t)
	s := newTestStore(t)
	if err := IndexOpenCode(s, ocPath, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RebuildFacts(); err != nil {
		t.Fatal(err)
	}
	cube, err := s.Cube()
	if err != nil {
		t.Fatal(err)
	}
	if len(cube) == 0 {
		t.Fatal("empty cube")
	}
	for _, r := range cube {
		if r.Source != SourceOpenCode {
			t.Fatalf("cube row source=%s want opencode", r.Source)
		}
	}
	f := facetsFrom(cube)
	found := false
	for _, x := range f.Sources {
		if x == SourceOpenCode {
			found = true
		}
	}
	if !found {
		t.Fatalf("facets missing opencode source: %v", f.Sources)
	}
}

// A database written before the source column existed must be upgraded in place:
// existing rows become source='pi', and the pre-source facts table (whose
// PRIMARY KEY lacks source) is dropped and recreated for the rollup.
func TestMigrateSourceUpgradesOldSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE files (path TEXT PRIMARY KEY, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL,
			offset INTEGER NOT NULL DEFAULT 0, indexed_at TEXT NOT NULL);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT NOT NULL, project TEXT NOT NULL,
			kind TEXT NOT NULL, agent TEXT NOT NULL DEFAULT '', started TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '', msgs INTEGER NOT NULL DEFAULT 0, input INTEGER NOT NULL DEFAULT 0,
			output INTEGER NOT NULL DEFAULT 0, cache_read INTEGER NOT NULL DEFAULT 0,
			cache_write INTEGER NOT NULL DEFAULT 0, reasoning INTEGER NOT NULL DEFAULT 0,
			total INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL DEFAULT '',
			source_path TEXT NOT NULL, msg_id TEXT NOT NULL DEFAULT '', ts TEXT NOT NULL DEFAULT '',
			day TEXT NOT NULL, hour INTEGER NOT NULL DEFAULT 0, project TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
			input INTEGER NOT NULL DEFAULT 0, output INTEGER NOT NULL DEFAULT 0,
			cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0,
			reasoning INTEGER NOT NULL DEFAULT 0, total INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE seen_msg (msg_id TEXT NOT NULL, tok INTEGER NOT NULL, source_path TEXT NOT NULL,
			PRIMARY KEY (msg_id, tok, source_path));
		CREATE TABLE facts (day TEXT NOT NULL, hour INTEGER NOT NULL DEFAULT 0, project TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
			input INTEGER NOT NULL DEFAULT 0, output INTEGER NOT NULL DEFAULT 0,
			cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0,
			reasoning INTEGER NOT NULL DEFAULT 0, total INTEGER NOT NULL DEFAULT 0,
			msgs INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, hour, project, model, provider, kind));
		CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO sessions (id, cwd, project, kind) VALUES ('s1', '/home/u/p', 'p', 'main');
		INSERT INTO usage_events (session_id, source_path, day, project, kind, total)
			VALUES ('s1', '/home/u/.pi/agent/sessions/x.jsonl', '2026-01-02', 'p', 'main', 42);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, tbl := range []string{"sessions", "usage_events", "facts"} {
		has, err := s.hasColumn(tbl, "source")
		if err != nil || !has {
			t.Fatalf("%s missing source column after migration (has=%v err=%v)", tbl, has, err)
		}
	}
	var src string
	if err := s.db.QueryRow(`SELECT source FROM usage_events`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != SourcePi {
		t.Fatalf("legacy row source=%s want pi", src)
	}
	// The rollup must still work against the recreated facts table.
	if err := s.RebuildFacts(); err != nil {
		t.Fatal(err)
	}
	var total int
	s.db.QueryRow(`SELECT total FROM facts WHERE source='pi'`).Scan(&total)
	if total != 42 {
		t.Fatalf("rolled up total=%d want 42", total)
	}
}
