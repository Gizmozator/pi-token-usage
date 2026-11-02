package pius

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// hermes-agent keeps its token accounting in ~/.hermes/state.db. Unlike
// opencode's append-only message log, session_model_usage holds one row per
// (session, model, billing..., task) whose counters are CUMULATIVE: they only
// grow while the session lives and are updated in place. There is no per-call
// log to tail, so ingestion is delta-based:
//
//   - cum_state stores the per-field totals already ingested for each usage key;
//   - each run inserts the growth since the last run as one usage_event,
//     stamped at the row's last_seen (when the newest of those tokens was spent);
//   - counters that shrink (session rewind/reset) reset the baseline without
//     emitting negative events.
//
// The source is tagged source=hermes and gets its own UI filter, like pi and
// opencode.

// hermesKeySep separates the db path from the usage key inside cum_state keys.
const hermesKeySep = "\x1f"

// hermesTotalMetaPrefix caches the ingested grand total per db path so the
// page-load staleness check is two integer reads, not a scan.
const hermesTotalMetaPrefix = "hermes_cumsum:"

// hermesRow is one session_model_usage row joined with its session metadata.
type hermesRow struct {
	usageKey   string
	sessionID  string
	model      string
	provider   string
	input      int
	output     int
	cacheRead  int
	cacheWrite int
	reasoning  int
	lastSeen   float64
	cwd        string
	parentID   string
	src        string
	startedAt  float64
}

func (r *hermesRow) total() int {
	return r.input + r.output + r.cacheRead + r.cacheWrite + r.reasoning
}

// hermesCumPrefix is the cum_state key prefix for one hermes database.
func hermesCumPrefix(dbPath string) string {
	return dbPath + hermesKeySep
}

// IndexHermes ingests the growth of session_model_usage from the hermes-agent
// state database at dbPath into the shared index, tagged with source=hermes.
// A missing db is not an error: machines without hermes contribute nothing.
func IndexHermes(s *Store, dbPath string, res *IndexResult, full bool) error {
	if dbPath == "" {
		return nil
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil
	}

	db, err := openOpenCode(dbPath) // generic read-only SQLite opener (WAL-safe)
	if err != nil {
		return err
	}
	defer db.Close()

	// A hermes install without the usage table (too old / too new): no-op.
	var hasTable int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_model_usage'`).Scan(&hasTable); err != nil {
		return fmt.Errorf("hermes db %s: %w", dbPath, err)
	}
	if hasTable == 0 {
		return nil
	}

	rows, err := db.Query(`
		SELECT u.session_id, u.model, u.billing_provider, u.billing_base_url,
		       u.billing_mode, u.task,
		       u.input_tokens, u.output_tokens, u.cache_read_tokens,
		       u.cache_write_tokens, u.reasoning_tokens,
		       COALESCE(u.last_seen, 0),
		       COALESCE(s.cwd, ''), COALESCE(s.parent_session_id, ''),
		       COALESCE(s.source, ''), COALESCE(s.started_at, 0)
		FROM session_model_usage u
		LEFT JOIN sessions s ON s.id = u.session_id`)
	if err != nil {
		return fmt.Errorf("hermes db %s: %w", dbPath, err)
	}
	defer rows.Close()

	var usage []hermesRow
	prefix := hermesCumPrefix(dbPath)
	for rows.Next() {
		var r hermesRow
		var base, mode, task string
		if err := rows.Scan(&r.sessionID, &r.model, &r.provider, &base,
			&mode, &task,
			&r.input, &r.output, &r.cacheRead, &r.cacheWrite, &r.reasoning,
			&r.lastSeen, &r.cwd, &r.parentID, &r.src, &r.startedAt); err != nil {
			return err
		}
		// The usage key mirrors session_model_usage's PRIMARY KEY, prefixed
		// with the db path so several hermes databases can share cum_state.
		r.usageKey = strings.Join([]string{r.sessionID, r.model, r.provider, base, mode, task}, "|")
		usage = append(usage, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	// Baselines already ingested for this db.
	stored := map[string][5]int{}
	srows, err := s.db.Query(`SELECT key, input, output, cache_read, cache_write, reasoning
		FROM cum_state WHERE substr(key, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return err
	}
	for srows.Next() {
		var key string
		var v [5]int
		if err := srows.Scan(&key, &v[0], &v[1], &v[2], &v[3], &v[4]); err != nil {
			srows.Close()
			return err
		}
		stored[strings.TrimPrefix(key, prefix)] = v
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		return err
	}
	srows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if full {
		if err := s.ResetFile(tx, dbPath); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`DELETE FROM cum_state WHERE substr(key, 1, ?) = ?`, len(prefix), prefix); err != nil {
			tx.Rollback()
			return err
		}
		stored = map[string][5]int{}
	}

	upsertCum, err := tx.Prepare(`INSERT INTO cum_state (key, input, output, cache_read, cache_write, reasoning)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
			input=excluded.input, output=excluded.output,
			cache_read=excluded.cache_read, cache_write=excluded.cache_write,
			reasoning=excluded.reasoning`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer upsertCum.Close()

	insEvent, err := tx.Prepare(`INSERT INTO usage_events
		(session_id, source_path, msg_id, ts, day, hour, project, model, provider, kind, source,
		 input, output, cache_read, cache_write, reasoning, total)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer insEvent.Close()

	seenSess := map[string]bool{}
	added := 0
	newTotal := 0
	for _, r := range usage {
		cur := [5]int{r.input, r.output, r.cacheRead, r.cacheWrite, r.reasoning}
		prev := stored[r.usageKey]

		// Per-field growth; a shrink resets the baseline silently.
		var d [5]int
		delta := 0
		for i := range d {
			d[i] = cur[i] - prev[i]
			if d[i] < 0 {
				d[i] = 0
			}
			delta += d[i]
		}
		stored[r.usageKey] = cur
		newTotal += r.total()

		kind := KindMain
		if r.parentID != "" || r.src == "subagent" {
			kind = KindSubagent
		}
		project := "?"
		if r.cwd != "" {
			project = projectFromCWD(r.cwd)
		}

		// Session meta for every usage row, even ones with no new tokens,
		// so the session exists for the aggregate refresh.
		if !seenSess[r.sessionID] {
			seenSess[r.sessionID] = true
			meta := SessionMeta{
				ID:      r.sessionID,
				CWD:     r.cwd,
				Project: project,
				Kind:    kind,
				Agent:   "hermes",
				Path:    dbPath,
				Source:  SourceHermes,
			}
			if r.startedAt > 0 {
				meta.Started = time.Unix(int64(r.startedAt), 0).UTC().Format(time.RFC3339)
			}
			if err := s.upsertSessionMeta(tx, meta); err != nil {
				tx.Rollback()
				return err
			}
		}

		if _, err := upsertCum.Exec(prefix+r.usageKey, cur[0], cur[1], cur[2], cur[3], cur[4]); err != nil {
			tx.Rollback()
			return err
		}
		if delta == 0 {
			continue
		}

		// Stamp at last_seen (newest token of this row), falling back to the
		// session start.
		secs := r.lastSeen
		if secs == 0 {
			secs = r.startedAt
		}
		day, hour := "unknown", 0
		ts := ""
		if secs > 0 {
			local := time.Unix(int64(secs), 0).In(locOrUTC)
			day = local.Format("2006-01-02")
			hour = local.Hour()
			ts = local.UTC().Format(time.RFC3339)
		}

		provider := r.provider
		if provider == "" {
			provider = "hermes"
		}
		// msg_id is unique per growth step of the usage key: the key itself
		// plus the new cumulative total.
		msgID := fmt.Sprintf("hermes:%s:%d", r.usageKey, r.total())

		if _, err := insEvent.Exec(r.sessionID, dbPath, msgID, ts, day, hour,
			project, r.model, provider, kind, SourceHermes,
			d[0], d[1], d[2], d[3], d[4], delta); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := upsertCum.Exec(prefix+r.usageKey, cur[0], cur[1], cur[2], cur[3], cur[4]); err != nil {
			tx.Rollback()
			return err
		}
		added++
	}

	_, err = tx.Exec(`INSERT INTO files (path, size, mtime_ns, offset, indexed_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			size=excluded.size, mtime_ns=excluded.mtime_ns,
			offset=excluded.offset, indexed_at=excluded.indexed_at`,
		dbPath, info.Size(), info.ModTime().UnixNano(), int64(newTotal), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.SetMeta(hermesTotalMetaPrefix+dbPath, strconv.Itoa(newTotal)); err != nil {
		return err
	}

	res.FilesScanned++
	res.EventsAdded += added
	res.BytesRead += info.Size()
	return nil
}

// hermesGrandTotal sums every counter in session_model_usage.
func hermesGrandTotal(dbPath string) (int64, error) {
	db, err := openOpenCode(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int64
	err = db.QueryRow(`SELECT COALESCE(sum(input_tokens + output_tokens + cache_read_tokens +
		cache_write_tokens + reasoning_tokens), 0) FROM session_model_usage`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("hermes db %s: %w", dbPath, err)
	}
	return n, nil
}

// hermesChanged reports whether the hermes usage counters grew since the last
// ingest. Unreadable or absent databases report "unchanged": they must never
// wedge the page-load freshness check.
func hermesChanged(s *Store, dbPath string) (bool, error) {
	if dbPath == "" {
		return false, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return false, nil
	}
	cur, err := hermesGrandTotal(dbPath)
	if err != nil {
		return false, nil //nolint:nilerr // deliberate: skip, don't fail the page
	}
	stored, err := strconv.Atoi(s.GetMeta(hermesTotalMetaPrefix + dbPath))
	if err != nil {
		return cur > 0, nil // never ingested (or legacy state): compare against zero
	}
	return int64(stored) != cur, nil
}
