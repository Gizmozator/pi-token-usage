package pius

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// createOpenCodeV2DB builds a minimal stand-in for opencode v2's storage:
// session_v2/session_message with the columns and data shapes the indexer reads.
func createOpenCodeV2DB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE session_v2 (
			id text PRIMARY KEY,
			parent_id text,
			directory text NOT NULL,
			agent text,
			time_created integer NOT NULL
		);
		CREATE TABLE session_message (
			id text PRIMARY KEY,
			session_id text NOT NULL,
			type text NOT NULL,
			data text NOT NULL
		);`); err != nil {
		t.Fatal(err)
	}
	return p
}

func ocV2InsertSession(t *testing.T, db *sql.DB, id, parent, dir, agent string, createdMS int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO session_v2 (id, parent_id, directory, agent, time_created) VALUES (?,?,?,?,?)`,
		id, parent, dir, agent, createdMS)
	if err != nil {
		t.Fatal(err)
	}
}

// ocV2InsertMsg writes a v2 message. v2 nests the model as an object and has no
// tokens.total field.
func ocV2InsertMsg(t *testing.T, db *sql.DB, id, sessionID, typ, model, provider string, completedMS int64, in, out, reasoning, cacheR int) {
	t.Helper()
	data := map[string]any{
		"agent": "build",
		"model": map[string]any{"id": model, "providerID": provider},
		"time":  map[string]any{"created": completedMS - 1000, "completed": completedMS},
		"tokens": map[string]any{
			"input":     in,
			"output":    out,
			"reasoning": reasoning,
			"cache":     map[string]any{"read": cacheR, "write": 0},
		},
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_message (id, session_id, type, data) VALUES (?,?,?,?)`,
		id, sessionID, typ, string(b)); err != nil {
		t.Fatal(err)
	}
}

func seedOpenCodeV2(t *testing.T) string {
	t.Helper()
	p := createOpenCodeV2DB(t)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC).UnixMilli()
	ocV2InsertSession(t, db, "ses_1", "", "/home/u/projA", "build", ts)
	ocV2InsertSession(t, db, "ses_2", "ses_1", "/home/u/projB", "general", ts)
	completed := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC).UnixMilli()
	ocV2InsertMsg(t, db, "msg_1", "ses_1", "assistant", "model-a", "prov-x", completed, 100, 40, 10, 500)
	ocV2InsertMsg(t, db, "msg_2", "ses_1", "user", "", "", completed, 0, 0, 0, 0)
	ocV2InsertMsg(t, db, "msg_3", "ses_2", "assistant", "model-b", "prov-y", completed+5000, 5, 7, 0, 0)
	ocV2InsertMsg(t, db, "msg_4", "ses_2", "assistant", "model-b", "prov-y", completed+6000, 0, 0, 0, 0) // zero tokens
	return p
}

func TestOpenCodeV2Basic(t *testing.T) {
	ocPath := seedOpenCodeV2(t)
	s := newTestStore(t)

	res := &IndexResult{}
	if err := IndexOpenCode(s, ocPath, res, false); err != nil {
		t.Fatal(err)
	}
	if res.EventsAdded != 2 {
		t.Fatalf("events added=%d want 2 (user and zero-token rows excluded)", res.EventsAdded)
	}

	var src, project, kind, model, provider, day string
	var hour, total, reasoning int
	err := s.db.QueryRow(`SELECT source, project, kind, model, provider, day, hour, total, reasoning
		FROM usage_events WHERE msg_id='msg_1'`).
		Scan(&src, &project, &kind, &model, &provider, &day, &hour, &total, &reasoning)
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceOpenCode || project != "projA" || kind != string(KindMain) || model != "model-a" || provider != "prov-x" {
		t.Fatalf("msg_1: src=%s project=%s kind=%s model=%s provider=%s", src, project, kind, model, provider)
	}
	// v2 has no total field: total must be the component sum 100+40+10+500.
	if day != "2026-01-02" || hour != 10 || total != 650 || reasoning != 10 {
		t.Fatalf("msg_1 bucketing: day=%s hour=%d total=%d reasoning=%d", day, hour, total, reasoning)
	}

	var sessSrc string
	if err := s.db.QueryRow(`SELECT source FROM sessions WHERE id='ses_2'`).Scan(&sessSrc); err != nil {
		t.Fatal(err)
	}
	if sessSrc != SourceOpenCode {
		t.Fatalf("session source=%s want opencode", sessSrc)
	}
}

func TestOpenCodeV2IncrementalTail(t *testing.T) {
	ocPath := seedOpenCodeV2(t)
	s := newTestStore(t)
	if err := IndexOpenCode(s, ocPath, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", ocPath)
	if err != nil {
		t.Fatal(err)
	}
	ocV2InsertMsg(t, db, "msg_5", "ses_1", "assistant", "model-a", "prov-x",
		time.Date(2026, 1, 3, 8, 0, 0, 0, time.UTC).UnixMilli(), 9, 1, 0, 0)
	db.Close()

	res := &IndexResult{}
	if err := IndexOpenCode(s, ocPath, res, false); err != nil {
		t.Fatal(err)
	}
	if res.EventsAdded != 1 {
		t.Fatalf("tail added=%d want 1", res.EventsAdded)
	}
	res2 := &IndexResult{}
	if err := IndexOpenCode(s, ocPath, res2, false); err != nil {
		t.Fatal(err)
	}
	if res2.EventsAdded != 0 || res2.FilesSkipped != 1 {
		t.Fatalf("no-op run: added=%d skipped=%d want 0/1", res2.EventsAdded, res2.FilesSkipped)
	}
}

// The real v1 -> v2 migration copies old messages into session_message. When
// the storage generation switches under an existing index, the v1 events must
// be replaced (not appended to), so the overlap is counted exactly once.
func TestOpenCodeV1ToV2SwitchNoDoubleCount(t *testing.T) {
	// Start with a v1 db and index it.
	ocPath := seedOpenCode(t)
	s := newTestStore(t)
	if err := IndexOpenCode(s, ocPath, &IndexResult{}, false); err != nil {
		t.Fatal(err)
	}

	// Migrate: add v2 tables containing the same two counted messages plus a
	// new one (what opencode's own migration produces).
	db, err := sql.Open("sqlite", ocPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE session_v2 (id text PRIMARY KEY, parent_id text, directory text NOT NULL,
			agent text, time_created integer NOT NULL);
		CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL,
			type text NOT NULL, data text NOT NULL);
		INSERT INTO session_v2 SELECT id, parent_id, directory, agent, time_created FROM session;`); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC).UnixMilli()
	completed := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC).UnixMilli()
	ocV2InsertMsg(t, db, "msg_1", "ses_1", "assistant", "model-a", "prov-x", completed, 100, 40, 0, 500) // total 640 incl.
	ocV2InsertMsg(t, db, "msg_3", "ses_2", "assistant", "model-b", "prov-y", completed+5000, 5, 7, 0, 0)
	ocV2InsertMsg(t, db, "msg_5", "ses_1", "assistant", "model-a", "prov-x",
		time.Date(2026, 1, 3, 8, 0, 0, 0, time.UTC).UnixMilli(), 9, 1, 0, 0)
	_ = ts
	db.Close()

	res := &IndexResult{}
	if err := IndexOpenCode(s, ocPath, res, false); err != nil {
		t.Fatal(err)
	}

	var n int
	var sum int64
	if err := s.db.QueryRow(`SELECT count(*), COALESCE(sum(total),0) FROM usage_events WHERE source='opencode'`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	// Exactly the three v2 messages: 640 + 12 + 10. The two migrated ones
	// must not be double-counted with their v1 rows.
	if n != 3 || sum != 662 {
		t.Fatalf("after v1->v2 switch: events=%d sum=%d want 3/662", n, sum)
	}
}
