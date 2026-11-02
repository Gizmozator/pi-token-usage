package pius

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"
)

// opencode keeps its state in a SQLite database (~/.local/share/opencode/opencode.db).
// Assistant messages carry per-message token accounting in a JSON blob, which maps
// one-to-one onto our UsageEvent: input/output/reasoning plus a cache read/write
// pair. Sessions carry the working directory (-> project) and a parent id for
// subagent runs (-> kind).
//
// Two storage generations live in that one file:
//
//	v1: session / message          (frozen once opencode migrated to v2)
//	v2: session_v2 / session_message (type column marks the role; v1 history
//	    was migrated into it, so it is the single source of truth when present)
//
// Reading both would double-count the migrated overlap, so the active schema is
// detected from the presence of session_v2 and recorded in meta. A schema
// switch forces a full re-ingest (ResetFile first) so every message is counted
// exactly once across the transition. Both generations ingest as source=opencode.
//
// Ingestion is incremental the same way the JSONL pass is: the `files` table
// records the highest message rowid consumed, and the next run reads only rows
// past that offset. opencode appends messages; it never rewrites older ones, so
// a rowid tail is safe.

// ocSchemaMetaPrefix keys the recorded storage generation per db path.
const ocSchemaMetaPrefix = "opencode_schema:"

// ocSession mirrors the columns we read from the session table (both schemas).
type ocSession struct {
	Directory string
	ParentID  string
	Agent     string
	CreatedMS int64
}

// ocMessageV1 mirrors the JSON stored in message.data for assistant messages.
type ocMessageV1 struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Agent      string `json:"agent"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Total     int `json:"total"`
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// ocMessageV2 mirrors the JSON stored in session_message.data for rows with
// type='assistant'. v2 has no total field and nests the model as an object.
type ocMessageV2 struct {
	Agent string `json:"agent"`
	Model struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// ocUsage is the schema-normalised view of one counted assistant message.
type ocUsage struct {
	model      string
	provider   string
	created    int64
	completed  int64
	input      int
	output     int
	reasoning  int
	cacheRead  int
	cacheWrite int
	total      int
}

// openOpenCode opens an opencode database read-only. The database is in WAL mode
// and normally held open by the opencode process; a read-only connection works as
// long as the -shm sidecar is readable, which it is while opencode runs and after
// a clean shutdown. Ping forces the failure to surface here rather than mid-query.
func openOpenCode(dbPath string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: dbPath}
	u.RawQuery = "mode=ro&_pragma=busy_timeout(3000)"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, err
}

// ocIsV2 reports whether the database uses the v2 (session_v2/session_message)
// storage generation.
func ocIsV2(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_v2'`).Scan(&n)
	return n > 0, err
}

// ocTables returns the session and message table names for the active schema.
func ocTables(v2 bool) (sessionTable, messageTable string) {
	if v2 {
		return "session_v2", "session_message"
	}
	return "session", "message"
}

// OpenCodeMaxRowID reports the highest message rowid in an opencode database,
// in whichever message table the active schema uses. Used by the cheap
// staleness check.
func OpenCodeMaxRowID(dbPath string) (int64, error) {
	db, err := openOpenCode(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	v2, err := ocIsV2(db)
	if err != nil {
		return 0, err
	}
	_, msgTable := ocTables(v2)
	var n int64
	if err := db.QueryRow(`SELECT COALESCE(max(rowid), 0) FROM ` + msgTable).Scan(&n); err != nil {
		return 0, fmt.Errorf("opencode db %s: %w", dbPath, err)
	}
	return n, nil
}

// IndexOpenCode ingests assistant messages from the opencode database at dbPath
// into the shared index, tagged with source=opencode. A missing db is not an
// error: machines without opencode simply contribute nothing.
//
// Tail rule mirrors the JSONL pass:
//
//	schema changed     -> storage generation switched -> full re-ingest
//	maxRowid == offset -> nothing new, skip
//	maxRowid >  offset -> read only rows past the offset
//	maxRowid <  offset -> rows were deleted -> full re-ingest
func IndexOpenCode(s *Store, dbPath string, res *IndexResult, full bool) error {
	if dbPath == "" {
		return nil
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil
	}

	db, err := openOpenCode(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	v2, err := ocIsV2(db)
	if err != nil {
		return fmt.Errorf("opencode db %s: schema: %w", dbPath, err)
	}
	sessTable, msgTable := ocTables(v2)
	marker := "v1"
	if v2 {
		marker = "v2"
	}
	schemaKey := ocSchemaMetaPrefix + dbPath

	var maxRowID int64
	if err := db.QueryRow(`SELECT COALESCE(max(rowid), 0) FROM ` + msgTable).Scan(&maxRowID); err != nil {
		return fmt.Errorf("opencode db %s: %w", dbPath, err)
	}

	state, err := s.GetFile(dbPath)
	if err != nil {
		return err
	}

	offset := int64(0)
	needFull := full
	schemaChanged := s.GetMeta(schemaKey) != marker
	switch {
	case !state.Exists:
		needFull = true
	case schemaChanged:
		// v1 rows were migrated into the v2 tables: re-ingest from scratch so
		// the overlap is counted exactly once (ResetFile drops the v1 events).
		needFull = true
	case maxRowID == state.Offset && !full:
		res.FilesSkipped++
		return nil
	case full || state.Offset > maxRowID || state.Offset <= 0:
		needFull = true
	default:
		offset = state.Offset
	}

	sessions, err := loadOpenCodeSessions(db, sessTable)
	if err != nil {
		return err
	}

	msgQuery := fmt.Sprintf(
		`SELECT rowid, id, session_id, data FROM %s WHERE rowid > ? ORDER BY rowid`, msgTable)
	if v2 {
		msgQuery = fmt.Sprintf(
			`SELECT rowid, id, session_id, data FROM %s WHERE type='assistant' AND rowid > ? ORDER BY rowid`, msgTable)
	}
	rows, err := db.Query(msgQuery, offset)
	if err != nil {
		return fmt.Errorf("opencode db %s: %w", dbPath, err)
	}
	defer rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if needFull && state.Exists {
		if err := s.ResetFile(tx, dbPath); err != nil {
			tx.Rollback()
			return err
		}
	}

	// Session metadata first so every event's session exists for the
	// aggregate refresh, regardless of whether it produced counted messages.
	for id, ses := range sessions {
		meta := SessionMeta{
			ID:      id,
			CWD:     ses.Directory,
			Project: projectFromCWD(ses.Directory),
			Kind:    KindMain,
			Agent:   ses.Agent,
			Path:    dbPath,
			Source:  SourceOpenCode,
		}
		if meta.Agent == "" {
			meta.Agent = "opencode"
		}
		if ses.ParentID != "" {
			meta.Kind = KindSubagent
		}
		if ses.CreatedMS > 0 {
			meta.Started = time.UnixMilli(ses.CreatedMS).UTC().Format(time.RFC3339)
		}
		if err := s.upsertSessionMeta(tx, meta); err != nil {
			tx.Rollback()
			return err
		}
	}

	insEvent, err := tx.Prepare(`INSERT INTO usage_events
		(session_id, source_path, msg_id, ts, day, hour, project, model, provider, kind, source,
		 input, output, cache_read, cache_write, reasoning, total)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer insEvent.Close()

	insSeen, err := tx.Prepare(`INSERT OR IGNORE INTO seen_msg (msg_id, tok, source_path) VALUES (?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer insSeen.Close()

	dup, err := tx.Prepare(`SELECT 1 FROM seen_msg WHERE msg_id = ? AND tok = ? AND source_path != ? LIMIT 1`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer dup.Close()

	added, skipped := 0, 0
	for rows.Next() {
		var (
			rowID     int64
			msgID     string
			sessionID string
			data      string
		)
		if err := rows.Scan(&rowID, &msgID, &sessionID, &data); err != nil {
			tx.Rollback()
			return err
		}
		u, ok := parseOCMessage(data, v2)
		if !ok {
			continue
		}

		// Prefer completion time: it is when the tokens were actually spent.
		ms := u.completed
		if ms == 0 {
			ms = u.created
		}
		day, hour := "unknown", 0
		ts := ""
		if ms > 0 {
			local := time.UnixMilli(ms).In(locOrUTC)
			day = local.Format("2006-01-02")
			hour = local.Hour()
			ts = local.UTC().Format(time.RFC3339)
		}

		kind := KindMain
		project := "?"
		if ses, ok := sessions[sessionID]; ok {
			project = projectFromCWD(ses.Directory)
			if ses.ParentID != "" {
				kind = KindSubagent
			}
		}

		if msgID != "" {
			var one int
			err := dup.QueryRow(msgID, u.total, dbPath).Scan(&one)
			if err == nil {
				skipped++
				continue
			}
			if err != sql.ErrNoRows {
				tx.Rollback()
				return err
			}
		}
		if _, err := insEvent.Exec(sessionID, dbPath, msgID, ts, day, hour,
			project, u.model, u.provider, kind, SourceOpenCode,
			u.input, u.output, u.cacheRead, u.cacheWrite, u.reasoning, u.total); err != nil {
			tx.Rollback()
			return err
		}
		if msgID != "" {
			if _, err := insSeen.Exec(msgID, u.total, dbPath); err != nil {
				tx.Rollback()
				return err
			}
		}
		added++
	}
	if err := rows.Err(); err != nil {
		tx.Rollback()
		return err
	}
	rows.Close()

	_, err = tx.Exec(`INSERT INTO files (path, size, mtime_ns, offset, indexed_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			size=excluded.size, mtime_ns=excluded.mtime_ns,
			offset=excluded.offset, indexed_at=excluded.indexed_at`,
		dbPath, info.Size(), info.ModTime().UnixNano(), maxRowID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.SetMeta(schemaKey, marker); err != nil {
		return err
	}

	res.FilesScanned++
	res.EventsAdded += added
	res.EventsSkipped += skipped
	res.BytesRead += info.Size()
	return nil
}

// parseOCMessage normalises one message row into ocUsage. ok=false means the
// row is not a counted assistant message (wrong role, or zero tokens).
func parseOCMessage(data string, v2 bool) (*ocUsage, bool) {
	if v2 {
		var m ocMessageV2
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return nil, false
		}
		t := m.Tokens
		u := &ocUsage{
			model:      m.Model.ID,
			provider:   m.Model.ProviderID,
			created:    m.Time.Created,
			completed:  m.Time.Completed,
			input:      t.Input,
			output:     t.Output,
			reasoning:  t.Reasoning,
			cacheRead:  t.Cache.Read,
			cacheWrite: t.Cache.Write,
		}
		u.total = u.input + u.output + u.reasoning + u.cacheRead + u.cacheWrite
		if u.total == 0 {
			return nil, false
		}
		return u, true
	}
	var m ocMessageV1
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return nil, false
	}
	if m.Role != "assistant" {
		return nil, false
	}
	t := m.Tokens
	u := &ocUsage{
		model:      m.ModelID,
		provider:   m.ProviderID,
		created:    m.Time.Created,
		completed:  m.Time.Completed,
		input:      t.Input,
		output:     t.Output,
		reasoning:  t.Reasoning,
		cacheRead:  t.Cache.Read,
		cacheWrite: t.Cache.Write,
	}
	u.total = t.Total
	if u.total == 0 {
		u.total = u.input + u.output + u.reasoning + u.cacheRead + u.cacheWrite
	}
	if u.total == 0 {
		return nil, false
	}
	return u, true
}

func loadOpenCodeSessions(db *sql.DB, table string) (map[string]ocSession, error) {
	rows, err := db.Query(`SELECT id, directory, COALESCE(parent_id, ''), COALESCE(agent, ''), time_created FROM ` + table)
	if err != nil {
		return nil, fmt.Errorf("opencode sessions: %w", err)
	}
	defer rows.Close()

	out := map[string]ocSession{}
	for rows.Next() {
		var id string
		var s ocSession
		if err := rows.Scan(&id, &s.Directory, &s.ParentID, &s.Agent, &s.CreatedMS); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

// openCodeChanged reports whether the opencode db has rows we have not consumed.
// Unreadable or absent databases report "unchanged": they must never wedge the
// page-load freshness check.
func openCodeChanged(s *Store, dbPath string) (bool, error) {
	if dbPath == "" {
		return false, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return false, nil
	}
	maxID, marker, err := ocState(dbPath)
	if err != nil {
		return false, nil //nolint:nilerr // deliberate: skip, don't fail the page
	}
	state, err := s.GetFile(dbPath)
	if err != nil {
		return false, err
	}
	if !state.Exists {
		return maxID > 0, nil
	}
	// A storage-generation switch is a change even when the rowids happen to
	// line up: the tables behind the offset are different.
	if s.GetMeta(ocSchemaMetaPrefix+dbPath) != marker {
		return true, nil
	}
	return maxID != state.Offset, nil
}

// ocState reads the active schema's marker and highest message rowid in one
// read-only connection.
func ocState(dbPath string) (maxRowID int64, marker string, err error) {
	db, err := openOpenCode(dbPath)
	if err != nil {
		return 0, "", err
	}
	defer db.Close()
	v2, err := ocIsV2(db)
	if err != nil {
		return 0, "", err
	}
	marker = "v1"
	if v2 {
		marker = "v2"
	}
	_, msgTable := ocTables(v2)
	if err := db.QueryRow(`SELECT COALESCE(max(rowid), 0) FROM ` + msgTable).Scan(&maxRowID); err != nil {
		return 0, "", fmt.Errorf("opencode db %s: %w", dbPath, err)
	}
	return maxRowID, marker, nil
}
