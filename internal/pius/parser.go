package pius

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Source identifies which agent tool produced an event. The UI exposes it as a
// filter so pi and opencode usage can be viewed together or apart.
const (
	SourcePi       = "pi"
	SourceOpenCode = "opencode"
	SourceOCR      = "ocr"
)

// Kind classifies a session file by its position in the sessions tree.
type Kind string

const (
	// KindMain is a top-level session: <project>/<ts>_<uuid>.jsonl
	KindMain Kind = "main"
	// KindFork is a fork of a session: <project>/<stem>/forks/<ts>_<uuid>.jsonl
	KindFork Kind = "fork"
	// KindSubagent is a subagent run: <project>/<stem>/<uuid>/run-N/session.jsonl
	KindSubagent Kind = "subagent"
)

// SessionMeta describes a single session file.
type SessionMeta struct {
	ID      string `json:"id"`
	CWD     string `json:"cwd"`
	Project string `json:"project"`
	Kind    Kind   `json:"kind"`
	Agent   string `json:"agent"`
	Started string `json:"started"`
	Path    string `json:"path"`
	// Source is the tool that produced the session; empty means SourcePi.
	Source string `json:"source"`
}

// UsageEvent is one counted assistant message. This is the atomic unit of the
// index; every rollup is derived from these rows.
type UsageEvent struct {
	SessionID  string `json:"session_id"`
	SourcePath string `json:"source_path"`
	MsgID      string `json:"msg_id"`
	TS         string `json:"ts"`
	Day        string `json:"day"`
	Hour       int    `json:"hour"`
	Project    string `json:"project"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Kind       Kind   `json:"kind"`
	// Source is the tool that produced the event; empty means SourcePi.
	Source     string `json:"source"`
	Input      int    `json:"input"`
	Output     int    `json:"output"`
	CacheRead  int    `json:"cache_read"`
	CacheWrite int    `json:"cache_write"`
	Reasoning  int    `json:"reasoning"`
	Total      int    `json:"total"`
}

// ParseResult is what one file contributes to the index.
type ParseResult struct {
	Session SessionMeta
	Events  []UsageEvent
	// NextOffset is the byte offset of the last fully-parsed line boundary.
	// On a JSON error we stop before the offending line so a torn write
	// (pi appending while we read) never corrupts the index.
	NextOffset int64
	// Skipped counts messages excluded as duplicates of records already seen
	// in another file (fork copies).
	Skipped int
	// Malformed counts complete lines that failed to parse. They are skipped so
	// one bad record cannot stall the file.
	Malformed int
	// BytesScanned is the bytes actually read this pass.
	BytesScanned int64

	// loc is the bucketing zone for this parse; set by ParseFile, not serialised.
	loc *time.Location
}

// classify maps a session file path to its Kind.
func classify(path string) Kind {
	slash := filepath.ToSlash(path)
	switch {
	case strings.Contains(slash, "/forks/"):
		return KindFork
	case filepath.Base(slash) == "session.jsonl":
		return KindSubagent
	default:
		return KindMain
	}
}

// IsIgnored reports whether a path must be excluded from the index.
//
// subagent-artifacts/** are transcript mirrors of subagent runs that already live
// as nested session.jsonl files. They use a different envelope (recordType, not
// type) and would double-count ~850M tokens on this machine.
func IsIgnored(path string) bool {
	slash := filepath.ToSlash(path)
	return strings.Contains(slash, "subagent-artifacts/") ||
		strings.HasPrefix(filepath.Base(slash), ".")
}

type rawRecord struct {
	Type      string      `json:"type"`
	ID        string      `json:"id"`
	Timestamp string      `json:"timestamp"`
	CWD       string      `json:"cwd"`
	Name      string      `json:"name"`
	Message   *rawMessage `json:"message"`
}

type rawMessage struct {
	Role     string    `json:"role"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	Usage    *rawUsage `json:"usage"`
}

type rawUsage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cacheRead"`
	CacheWrite  int `json:"cacheWrite"`
	Reasoning   int `json:"reasoning"`
	TotalTokens int `json:"totalTokens"`
}

// ParseFile reads path starting at offset and returns the events it contributes.
//
// Byte accounting is exact and includes line terminators. This matters: the stored
// offset is the resume point for the next pass, so if it drifts even one byte the
// next read starts mid-line, every subsequent record fails to parse, and the file
// silently stops being indexed.
//
// Incomplete trailing data (pi appending while we read) is never committed: the
// offset stops before it and the next pass re-reads it once the write lands.
//
// loc controls day/hour bucketing: pass the user's local zone so late-night work
// lands on the correct day. A nil loc falls back to the package default.
func ParseFile(path string, offset int64, loc *time.Location) (*ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	res := &ParseResult{NextOffset: offset}
	res.Session = SessionMeta{
		Kind: classify(path),
		Path: path,
	}

	if loc == nil {
		loc = locOrUTC
	}
	res.loc = loc

	if offset >= st.Size() {
		return res, nil
	}
	// A resumed pass starts *after* the `session` header record, so the session
	// id and cwd are not among the bytes we are about to read. Seed them from the
	// top of the file first, otherwise every event added on a tail pass lands with
	// an empty session_id and project - which the UI renders as "(unknown)" until
	// someone forces a full re-parse.
	if offset > 0 {
		seedHeader(f, res)
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}

	next, scanned, err := scanJSONL(f, offset, func(body []byte, complete bool) (bool, error) {
		var rec rawRecord
		if jerr := json.Unmarshal(body, &rec); jerr != nil {
			if !complete {
				// Torn tail: stop before it so the next pass retries.
				return true, nil
			}
			// Complete but unparseable: skip this line, keep the file moving.
			res.Malformed++
			return false, nil
		}
		res.consume(&rec)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	res.NextOffset = next
	res.BytesScanned = scanned

	if res.Session.Project == "" && res.Session.CWD != "" {
		res.Session.Project = projectFromCWD(res.Session.CWD)
	}
	return res, nil
}

// scanJSONL walks newline-delimited records from f's current position, where
// offset is the byte position of the first byte being read. Every non-empty
// line is delivered to consume along with whether it was newline-terminated.
//
// The offset contract is shared by every envelope parser (pi, ocr) because it
// is the dangerous part: the returned offset is the end of the last delivered
// record and must include terminators. A torn trailing write (unterminated AND
// unparseable) is never committed - consume returns stop=true and the offset
// halts before it so the next pass retries. A complete-but-unparseable line is
// the consumer's problem: it counts the line malformed and the scan advances
// past it so one bad record cannot stall the file.
func scanJSONL(f *os.File, offset int64, consume func(body []byte, complete bool) (stop bool, err error)) (nextOffset, bytesScanned int64, err error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return offset, 0, err
	}

	cursor := offset
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')

		var body []byte
		var advance int
		complete := true
		if idx >= 0 {
			body = data[:idx]
			advance = idx + 1 // include the terminator in the resume offset
		} else {
			body = data
			advance = len(data)
			complete = false // no terminator: may be a write in flight
		}

		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 {
			stop, cerr := consume(trimmed, complete)
			if cerr != nil {
				return cursor, bytesScanned, cerr
			}
			if stop {
				return cursor, bytesScanned, nil
			}
		}

		data = data[advance:]
		cursor += int64(advance)
		bytesScanned += int64(advance)
	}
	return cursor, bytesScanned, nil
}

// consume folds one raw record into the parse result.
func (res *ParseResult) consume(rec *rawRecord) {
	switch rec.Type {
	case "session", "session_info":
		res.seedMeta(rec)
	case "message":
		res.addMessage(rec)
	}
}

// seedMeta folds the session-identifying records into the parse result and
// reports whether everything we look for is now present.
func (res *ParseResult) seedMeta(rec *rawRecord) bool {
	switch rec.Type {
	case "session":
		if res.Session.ID == "" {
			res.Session.ID = rec.ID
			res.Session.CWD = rec.CWD
			res.Session.Project = projectFromCWD(rec.CWD)
			res.Session.Started = rec.Timestamp
		}
	case "session_info":
		if res.Session.Agent == "" {
			res.Session.Agent = rec.Name
		}
	}
	return res.Session.ID != "" && res.Session.CWD != "" && res.Session.Agent != ""
}

const (
	// headerScanMaxLines bounds the resume header scan. Real headers are one or
	// two records; the cap only exists so a file with no header at all cannot make
	// us read the whole thing on every resume.
	headerScanMaxLines = 32
	// headerScanMaxBytes hard-caps the bytes touched while scanning for the header.
	headerScanMaxBytes = 256 * 1024
)

// seedHeader recovers session identity from the top of an already-open file
// without emitting any events. The caller re-seeks afterwards.
func seedHeader(f *os.File, res *ParseResult) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(io.LimitReader(f, headerScanMaxBytes))
	for i := 0; i < headerScanMaxLines; i++ {
		line, err := r.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var rec rawRecord
			if json.Unmarshal([]byte(trimmed), &rec) == nil && res.seedMeta(&rec) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// addMessage turns an assistant message's usage block into a UsageEvent.
func (res *ParseResult) addMessage(rec *rawRecord) {
	m := rec.Message
	if m == nil || m.Role != "assistant" {
		return
	}
	u := m.Usage
	if u == nil {
		return
	}
	total := u.TotalTokens
	if total == 0 && (u.Input != 0 || u.Output != 0 || u.CacheRead != 0 || u.CacheWrite != 0) {
		total = u.Input + u.Output + u.CacheRead + u.CacheWrite
	}
	if total == 0 {
		return
	}

	ts := parseTS(rec.Timestamp)
	day := "unknown"
	hour := 0
	if !ts.IsZero() {
		loc := res.loc
		if loc == nil {
			loc = locOrUTC
		}
		local := ts.In(loc)
		day = local.Format("2006-01-02")
		hour = local.Hour()
	}

	res.Events = append(res.Events, UsageEvent{
		SessionID:  res.Session.ID,
		SourcePath: res.Session.Path,
		MsgID:      rec.ID,
		TS:         rec.Timestamp,
		Day:        day,
		Hour:       hour,
		Project:    res.Session.Project,
		Model:      m.Model,
		Provider:   m.Provider,
		Kind:       res.Session.Kind,
		Input:      u.Input,
		Output:     u.Output,
		CacheRead:  u.CacheRead,
		CacheWrite: u.CacheWrite,
		Reasoning:  u.Reasoning,
		Total:      total,
	})
}

// locOrUTC is the bucketing zone; Index sets it from config. Kept package-level so
// ParseFile keeps a simple signature.
var locOrUTC = time.UTC

// SetLocation sets the day/hour bucketing zone for subsequent parses.
func SetLocation(loc *time.Location) {
	if loc == nil {
		loc = time.UTC
	}
	locOrUTC = loc
}

// parseTS accepts RFC3339 with or without fractional seconds.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// projectFromCWD derives a display project name from a working directory.
func projectFromCWD(cwd string) string {
	if cwd == "" {
		return "?"
	}
	clean := filepath.Clean(filepath.ToSlash(cwd))
	base := pathBase(clean)
	if base == "" || base == "/" || base == "." {
		return clean
	}
	return base
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
