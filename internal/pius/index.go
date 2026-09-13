package pius

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Discover lists indexable session files under root, excluding mirrors.
func Discover(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// Unreadable dirs (other users' homes, etc.) are not fatal.
			if os.IsPermission(err) {
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}
		if info.IsDir() {
			// The artifacts tree is a mirror; skip walking it at all.
			if info.Name() == "subagent-artifacts" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".jsonl" {
			return nil
		}
		if IsIgnored(p) {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Progress reports incremental index activity.
type Progress struct {
	FilesTotal   int
	FilesDone    int
	FilesChanged int
	EventsAdded  int
	BytesRead    int64
}

// IndexOptions controls an index run.
type IndexOptions struct {
	Root     string
	Full     bool
	OnDone   func(Progress)
}

// Index synchronises the database with the sessions directory.
//
// Trigger rule is SIZE, not mtime: rsync/restore/touch churn mtimes without new
// content, which would force pointless re-parses of hundreds of megabytes.
//
//	size == stored  -> skip (nothing new to read)
//	size >  stored -> tail-parse from the stored offset
//	size <  stored -> file was rewritten/compacted -> full re-parse of that file
func Index(s *Store, opts IndexOptions) (*IndexResult, error) {
	start := time.Now()
	files, err := Discover(opts.Root)
	if err != nil {
		return nil, err
	}

	res := &IndexResult{}
	if opts.Full {
		if err := fullReset(s); err != nil {
			return nil, err
		}
	}

	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		size := info.Size()
		mtime := info.ModTime().UnixNano()

		state, err := s.GetFile(path)
		if err != nil {
			return nil, err
		}

		offset := int64(0)
		needFull := opts.Full

		switch {
		case !state.Exists:
			needFull = true
		case size == state.Size:
			// Unchanged. This is the common case for ~all historical files.
			res.FilesSkipped++
			continue
		case size > state.Size && state.Offset > 0 && state.Offset <= size:
			offset = state.Offset
		default:
			// Shrank, or no usable offset: re-read from the top.
			needFull = true
		}

		parsed, err := ParseFile(path, offset, locOrUTC)
		if err != nil {
			// Unreadable file: record nothing, keep prior state intact.
			continue
		}

		tx, err := s.db.Begin()
		if err != nil {
			return nil, err
		}
		if needFull && state.Exists {
			if err := s.ResetFile(tx, path); err != nil {
				tx.Rollback()
				return nil, err
			}
		}
		if err := s.StoreFile(tx, parsed, size, mtime); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}

		res.FilesScanned++
		res.EventsAdded += len(parsed.Events) - parsed.Skipped
		res.EventsSkipped += parsed.Skipped
		res.BytesRead += parsed.BytesScanned

		if opts.OnDone != nil {
			opts.OnDone(Progress{
				FilesTotal:   len(files),
				FilesDone:    res.FilesScanned + res.FilesSkipped,
				FilesChanged: res.FilesScanned,
				EventsAdded:  res.EventsAdded,
				BytesRead:    res.BytesRead,
			})
		}
	}

	if err := s.RebuildFacts(); err != nil {
		return nil, err
	}
	if err := s.RefreshSessionAggregates(); err != nil {
		return nil, err
	}

	res.ElapsedMS = time.Since(start).Milliseconds()
	_ = s.SetMeta("last_index", time.Now().UTC().Format(time.RFC3339))
	return res, nil
}

// EnsureFresh performs a cheap staleness check and indexes only what changed.
// A full stat pass over the whole tree costs a few milliseconds, so this is safe
// to call on every page load.
func EnsureFresh(s *Store, root string) (*IndexResult, bool, error) {
	changed, err := anyChanged(s, root)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return &IndexResult{}, false, nil
	}
	r, err := Index(s, IndexOptions{Root: root})
	return r, true, err
}

// anyChanged reports whether any file differs in size from its recorded state.
func anyChanged(s *Store, root string) (bool, error) {
	files, err := Discover(root)
	if err != nil {
		return false, err
	}
	for _, p := range files {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		st, err := s.GetFile(p)
		if err != nil {
			return true, err
		}
		if !st.Exists || st.Size != info.Size() {
			return true, nil
		}
	}
	return false, nil
}

func fullReset(s *Store) error {
	_, err := s.db.Exec(`BEGIN IMMEDIATE;
		DELETE FROM usage_events;
		DELETE FROM seen_msg;
		DELETE FROM files;
		DELETE FROM sessions;
		DELETE FROM facts;
		COMMIT;`)
	return err
}

// Stats returns rough table sizes for the status endpoint.
func (s *Store) Stats() (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range []string{"files", "sessions", "usage_events", "facts"} {
		var n int64
		if err := s.db.QueryRow("SELECT count(*) FROM " + t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

var _ = sql.ErrNoRows // keep the sql import meaningful for future helpers
