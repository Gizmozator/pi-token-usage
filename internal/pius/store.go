package pius

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS files (
  path       TEXT PRIMARY KEY,
  size       INTEGER NOT NULL,
  mtime_ns   INTEGER NOT NULL,
  offset     INTEGER NOT NULL DEFAULT 0,
  indexed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id      TEXT PRIMARY KEY,
  cwd     TEXT NOT NULL,
  project TEXT NOT NULL,
  kind    TEXT NOT NULL,
  agent   TEXT NOT NULL DEFAULT '',
  started TEXT NOT NULL DEFAULT '',
  path    TEXT NOT NULL DEFAULT '',
  msgs    INTEGER NOT NULL DEFAULT 0,
  input   INTEGER NOT NULL DEFAULT 0,
  output  INTEGER NOT NULL DEFAULT 0,
  cache_read  INTEGER NOT NULL DEFAULT 0,
  cache_write INTEGER NOT NULL DEFAULT 0,
  reasoning   INTEGER NOT NULL DEFAULT 0,
  total   INTEGER NOT NULL DEFAULT 0
);

-- Atomic unit of the index: one row per counted assistant message.
CREATE TABLE IF NOT EXISTS usage_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL DEFAULT '',
  source_path TEXT NOT NULL,
  msg_id      TEXT NOT NULL DEFAULT '',
  ts          TEXT NOT NULL DEFAULT '',
  day         TEXT NOT NULL,
  hour        INTEGER NOT NULL DEFAULT 0,
  project     TEXT NOT NULL,
  model       TEXT NOT NULL DEFAULT '',
  provider    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL,
  input       INTEGER NOT NULL DEFAULT 0,
  output      INTEGER NOT NULL DEFAULT 0,
  cache_read  INTEGER NOT NULL DEFAULT 0,
  cache_write INTEGER NOT NULL DEFAULT 0,
  reasoning   INTEGER NOT NULL DEFAULT 0,
  total       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ev_day    ON usage_events(day);
CREATE INDEX IF NOT EXISTS idx_ev_proj   ON usage_events(project);
CREATE INDEX IF NOT EXISTS idx_ev_sess   ON usage_events(session_id);
CREATE INDEX IF NOT EXISTS idx_ev_source ON usage_events(source_path);

-- Fork-copy dedupe. A message is a duplicate only when the same (id, tokens)
-- appears in a DIFFERENT file; re-parsing a file must not dedupe against itself.
CREATE TABLE IF NOT EXISTS seen_msg (
  msg_id      TEXT NOT NULL,
  tok         INTEGER NOT NULL,
  source_path TEXT NOT NULL,
  PRIMARY KEY (msg_id, tok, source_path)
);
CREATE INDEX IF NOT EXISTS idx_seen_key ON seen_msg(msg_id, tok);

-- Materialised rollup, rebuilt from usage_events after every index run.
CREATE TABLE IF NOT EXISTS facts (
  day         TEXT NOT NULL,
  hour        INTEGER NOT NULL DEFAULT 0,
  project     TEXT NOT NULL,
  model       TEXT NOT NULL DEFAULT '',
  provider    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL,
  input       INTEGER NOT NULL DEFAULT 0,
  output      INTEGER NOT NULL DEFAULT 0,
  cache_read  INTEGER NOT NULL DEFAULT 0,
  cache_write INTEGER NOT NULL DEFAULT 0,
  reasoning   INTEGER NOT NULL DEFAULT 0,
  total       INTEGER NOT NULL DEFAULT 0,
  msgs        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, hour, project, model, provider, kind)
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	// temp_store=MEMORY is not a performance tweak, it is what keeps the container
	// alive: the facts rollup is an INSERT...SELECT with a GROUP BY, which needs a
	// temp file when temp_store is FILE. The scratch image ships no /tmp, and a
	// restarted container's tmpfs is not writable by uid 65534, so the sort fails
	// with "disk I/O error (6410)" (SQLITE_IOERR_ROLLBACK_ATOMIC) and the whole
	// index run dies. The rollup is ~1.3k groups, so keeping it in RAM is free.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite handles concurrent readers, but a single writer avoids
	// lock contention entirely during index runs.
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	return s.repairUnknownEvents()
}

// repairUnknownEvents heals the rows left behind by tail parses that ran before
// the parser seeded session identity from the file header: those events were
// written with an empty session_id and project and showed up as "(unknown)" in
// the UI.
//
// The columns are not patched in place - after the fact we no longer know which
// session each row belonged to. Instead the affected events are dropped along with
// their file offsets, so the next index pass re-reads those files from byte 0 and
// attributes them properly. Runs once (guarded by a meta flag) because the parser
// can no longer produce such rows.
func (s *Store) repairUnknownEvents() error {
	if s.GetMeta(repairUnknownKey) != "" {
		return nil
	}

	rows, err := s.db.Query(
		`SELECT DISTINCT source_path FROM usage_events WHERE session_id = '' OR project = ''`)
	if err != nil {
		return err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := tx.Exec(`DELETE FROM usage_events WHERE source_path = ?`, p); err != nil {
			tx.Rollback()
			return err
		}
		// Drop the dedupe keys and the resume offset too: without the latter the
		// file would never be re-read from the top.
		if _, err := tx.Exec(`DELETE FROM seen_msg WHERE source_path = ?`, p); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, p); err != nil {
			tx.Rollback()
			return err
		}
	}
	// Synthetic rows created when a parse produced no session id.
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id LIKE 'path:%'`); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.SetMeta(repairUnknownKey, time.Now().UTC().Format(time.RFC3339))
}

const repairUnknownKey = "repaired_unknown_events_v1"

// FileState is the recorded index state for one session file.
type FileState struct {
	Size     int64
	MTimeNS  int64
	Offset   int64
	Indexed  string
	Exists   bool
}

// GetFile returns the recorded state for path.
func (s *Store) GetFile(path string) (FileState, error) {
	var fs FileState
	err := s.db.QueryRow(
		`SELECT size, mtime_ns, offset, indexed_at FROM files WHERE path = ?`, path,
	).Scan(&fs.Size, &fs.MTimeNS, &fs.Offset, &fs.Indexed)
	if err == sql.ErrNoRows {
		return FileState{}, nil
	}
	if err != nil {
		return FileState{}, err
	}
	fs.Exists = true
	return fs, nil
}

// ResetFile drops everything derived from one source file, preparing for a full
// re-parse. Without this, a rewritten/compacted file would double-count.
func (s *Store) ResetFile(tx *sql.Tx, path string) error {
	if _, err := tx.Exec(`DELETE FROM usage_events WHERE source_path = ?`, path); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM seen_msg WHERE source_path = ?`, path)
	return err
}

// IndexResult reports what an index run did.
type IndexResult struct {
	FilesScanned  int
	FilesSkipped  int
	EventsAdded   int
	EventsSkipped int
	BytesRead     int64
	ElapsedMS     int64
}

// StoreFile persists the parse output for one session file inside tx.
func (s *Store) StoreFile(tx *sql.Tx, res *ParseResult, size, mtimeNS int64) error {
	if res.Session.ID == "" {
		res.Session.ID = "path:" + res.Session.Path
	}

	if err := s.upsertSessionMeta(tx, res.Session); err != nil {
		return err
	}

	insEvent, err := tx.Prepare(`INSERT INTO usage_events
		(session_id, source_path, msg_id, ts, day, hour, project, model, provider, kind,
		 input, output, cache_read, cache_write, reasoning, total)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insEvent.Close()

	insSeen, err := tx.Prepare(`INSERT OR IGNORE INTO seen_msg (msg_id, tok, source_path) VALUES (?,?,?)`)
	if err != nil {
		return err
	}
	defer insSeen.Close()

	dup, err := tx.Prepare(`SELECT 1 FROM seen_msg WHERE msg_id = ? AND tok = ? AND source_path != ? LIMIT 1`)
	if err != nil {
		return err
	}
	defer dup.Close()

	for _, ev := range res.Events {
		// Events are built during the parse. If the header had not been seen by the
		// time an event was created, stamp the identity we now know so no row is
		// ever written with an empty session_id or project.
		if ev.SessionID == "" {
			ev.SessionID = res.Session.ID
		}
		if ev.Project == "" {
			ev.Project = res.Session.Project
		}
		if ev.MsgID != "" {
			var one int
			err := dup.QueryRow(ev.MsgID, ev.Total, res.Session.Path).Scan(&one)
			if err == nil {
				res.Skipped++
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
		}
		if _, err := insEvent.Exec(ev.SessionID, ev.SourcePath, ev.MsgID, ev.TS, ev.Day, ev.Hour,
			ev.Project, ev.Model, ev.Provider, ev.Kind,
			ev.Input, ev.Output, ev.CacheRead, ev.CacheWrite, ev.Reasoning, ev.Total); err != nil {
			return err
		}
		if ev.MsgID != "" {
			if _, err := insSeen.Exec(ev.MsgID, ev.Total, res.Session.Path); err != nil {
				return err
			}
		}
	}

	_, err = tx.Exec(`INSERT INTO files (path, size, mtime_ns, offset, indexed_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			size=excluded.size, mtime_ns=excluded.mtime_ns,
			offset=excluded.offset, indexed_at=excluded.indexed_at`,
		res.Session.Path, size, mtimeNS, res.NextOffset, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) upsertSessionMeta(tx *sql.Tx, m SessionMeta) error {
	_, err := tx.Exec(`INSERT INTO sessions (id, cwd, project, kind, agent, started, path)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			cwd=excluded.cwd, project=excluded.project, kind=excluded.kind,
			agent=CASE WHEN excluded.agent != '' THEN excluded.agent ELSE sessions.agent END,
			started=CASE WHEN sessions.started = '' THEN excluded.started ELSE sessions.started END,
			path=excluded.path`,
		m.ID, m.CWD, m.Project, m.Kind, m.Agent, m.Started, m.Path)
	return err
}

// RebuildFacts recomputes the rollup from usage_events. Idempotent by
// construction: no drift, and a grain or timezone change needs no re-parse.
func (s *Store) RebuildFacts() error {
	_, err := s.db.Exec(`
		BEGIN IMMEDIATE;
		DELETE FROM facts;
		INSERT INTO facts (day, hour, project, model, provider, kind,
			input, output, cache_read, cache_write, reasoning, total, msgs)
		SELECT day, hour, project, model, provider, kind,
			sum(input), sum(output), sum(cache_read), sum(cache_write),
			sum(reasoning), sum(total), count(*)
		FROM usage_events GROUP BY day, hour, project, model, provider, kind;
		COMMIT;`)
	return err
}

// RefreshSessionAggregates folds event totals back onto the sessions rows.
func (s *Store) RefreshSessionAggregates() error {
	_, err := s.db.Exec(`
		UPDATE sessions SET
		  msgs        = COALESCE((SELECT count(*) FROM usage_events e WHERE e.session_id = sessions.id), 0),
		  input     = COALESCE((SELECT sum(input)     FROM usage_events e WHERE e.session_id = sessions.id), 0),
		  output    = COALESCE((SELECT sum(output)    FROM usage_events e WHERE e.session_id = sessions.id), 0),
		  cache_read= COALESCE((SELECT sum(cache_read)FROM usage_events e WHERE e.session_id = sessions.id), 0),
		  cache_write=COALESCE((SELECT sum(cache_write)FROM usage_events e WHERE e.session_id = sessions.id), 0),
		  reasoning = COALESCE((SELECT sum(reasoning) FROM usage_events e WHERE e.session_id = sessions.id), 0),
		  total     = COALESCE((SELECT sum(total)     FROM usage_events e WHERE e.session_id = sessions.id), 0);`)
	return err
}

// SetMeta stores an arbitrary key/value.
func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}

// GetMeta reads an arbitrary key/value.
func (s *Store) GetMeta(k string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, k).Scan(&v)
	return v
}

// Totals is the headline summary.
type Totals struct {
	TotalTokens int     `json:"total_tokens"`
	Input       int     `json:"input"`
	Output      int     `json:"output"`
	CacheRead   int     `json:"cache_read"`
	CacheWrite  int     `json:"cache_write"`
	Reasoning   int     `json:"reasoning"`
	Msgs        int     `json:"msgs"`
	Sessions    int     `json:"sessions"`
	Projects    int     `json:"projects"`
	Days        int     `json:"days"`
	CacheHit    float64 `json:"cache_hit"`
	FirstDay    string  `json:"first_day"`
	LastDay     string  `json:"last_day"`
}

// Totals computes headline numbers from the facts table.
func (s *Store) Totals() (Totals, error) {
	var t Totals
	err := s.db.QueryRow(`
		SELECT COALESCE(sum(total),0), COALESCE(sum(input),0), COALESCE(sum(output),0),
		       COALESCE(sum(cache_read),0), COALESCE(sum(cache_write),0),
		       COALESCE(sum(reasoning),0), COALESCE(sum(msgs),0),
		       min(day), max(day)
		FROM facts`).Scan(&t.TotalTokens, &t.Input, &t.Output, &t.CacheRead,
		&t.CacheWrite, &t.Reasoning, &t.Msgs, &t.FirstDay, &t.LastDay)
	if err != nil {
		return t, err
	}
	t.Sessions, _ = s.count(`SELECT count(*) FROM sessions`)
	t.Projects, _ = s.count(`SELECT count(DISTINCT project) FROM facts`)
	t.Days, _ = s.count(`SELECT count(DISTINCT day) FROM facts`)
	if d := t.Input + t.CacheRead; d > 0 {
		t.CacheHit = 100.0 * float64(t.CacheRead) / float64(d)
	}
	return t, nil
}

func (s *Store) count(q string) (int, error) {
	var n int
	err := s.db.QueryRow(q).Scan(&n)
	return n, err
}

// CubeRow is one cell of the rollup, shipped to the browser for client-side
// filtering. The whole cube is tiny (~1-3 KB gzipped) so we send all of it.
type CubeRow struct {
	Day        string `json:"d"`
	Hour       int    `json:"h"`
	Project    string `json:"p"`
	Model      string `json:"m"`
	Provider   string `json:"v"`
	Kind       string `json:"k"`
	Input      int    `json:"i"`
	Output     int    `json:"o"`
	CacheRead  int    `json:"c"`
	CacheWrite int    `json:"w"`
	Reasoning  int    `json:"r"`
	Total      int    `json:"t"`
	Msgs       int    `json:"n"`
}

// Cube returns the full rollup.
func (s *Store) Cube() ([]CubeRow, error) {
	rows, err := s.db.Query(`SELECT day, hour, project, model, provider, kind,
		input, output, cache_read, cache_write, reasoning, total, msgs
		FROM facts ORDER BY day, project, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CubeRow
	for rows.Next() {
		var r CubeRow
		if err := rows.Scan(&r.Day, &r.Hour, &r.Project, &r.Model, &r.Provider, &r.Kind,
			&r.Input, &r.Output, &r.CacheRead, &r.CacheWrite, &r.Reasoning, &r.Total, &r.Msgs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionRow is a drill-down entry.
type SessionRow struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Agent   string `json:"agent"`
	CWD     string `json:"cwd"`
	Started string `json:"started"`
	Msgs    int    `json:"msgs"`
	Total   int    `json:"total"`
	Input   int    `json:"input"`
	Output  int    `json:"output"`
}

// Sessions returns the session list, largest first.
func (s *Store) Sessions(limit int) ([]SessionRow, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT id, project, kind, agent, cwd, started, msgs, total, input, output
		FROM sessions ORDER BY total DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.Project, &r.Kind, &r.Agent, &r.CWD, &r.Started,
			&r.Msgs, &r.Total, &r.Input, &r.Output); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
