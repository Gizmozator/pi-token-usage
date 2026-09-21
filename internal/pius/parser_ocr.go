package pius

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// open-code-review (ocr) persists each review session as a JSONL transcript
// under ~/.opencodereview/sessions/<flattened-repo-path>/<session-uuid>.jsonl.
// The envelope differs from pi's (flat fields, snake_case usage), but the shape
// is the same kind of append-only log, so it rides the existing file-offset
// indexing machinery via scanJSONL.
//
// Only llm_response records carry token accounting:
//
//	{"type":"llm_response","sessionId":"...","uuid":"...","model":"...",
//	 "timestamp":"2026-09-20T19:44:38Z",
//	 "usage":{"prompt_tokens":8132,"completion_tokens":4695,
//	          "cache_read_tokens":0,"cache_write_tokens":0}}
//
// The other record types (llm_request with full message payloads, tool_call,
// llm_error, review_item_*) are skipped; session_start seeds identity.

type ocrRecord struct {
	Type      string    `json:"type"`
	UUID      string    `json:"uuid"`
	SessionID string    `json:"sessionId"`
	Timestamp string    `json:"timestamp"`
	CWD       string    `json:"cwd"`
	Model     string    `json:"model"`
	TaskType  string    `json:"taskType"`
	Usage     *ocrUsage `json:"usage"`
}

type ocrUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// ParseOCRFile reads an ocr session transcript starting at offset. Same
// offset/torn-tail contract as ParseFile; see scanJSONL. provider is the LLM
// provider name from ocr's config (records do not carry it); may be "".
func ParseOCRFile(path string, offset int64, loc *time.Location, provider string) (*ParseResult, error) {
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
		Kind:   KindMain,
		Path:   path,
		Source: SourceOCR,
		Agent:  "ocr",
	}

	if loc == nil {
		loc = locOrUTC
	}
	res.loc = loc

	if offset >= st.Size() {
		return res, nil
	}
	// Same trap as pi: a tail parse starts past the session_start header, so
	// seed identity from the top before resuming.
	if offset > 0 {
		seedOCRHeader(f, res)
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}

	next, scanned, err := scanJSONL(f, offset, func(body []byte, complete bool) (bool, error) {
		var rec ocrRecord
		if jerr := json.Unmarshal(body, &rec); jerr != nil {
			if !complete {
				return true, nil // torn tail: stop before it
			}
			res.Malformed++
			return false, nil
		}
		res.consumeOCR(&rec, provider)
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

// consumeOCR folds one ocr record into the parse result.
func (res *ParseResult) consumeOCR(rec *ocrRecord, provider string) {
	switch rec.Type {
	case "session_start":
		if res.Session.ID == "" && rec.SessionID != "" {
			res.Session.ID = rec.SessionID
			res.Session.Started = rec.Timestamp
		}
		if res.Session.CWD == "" && rec.CWD != "" {
			res.Session.CWD = rec.CWD
			res.Session.Project = projectFromCWD(rec.CWD)
		}
	case "llm_response":
		res.addOCRMessage(rec, provider)
	}
}

// addOCRMessage turns one llm_response's usage block into a UsageEvent.
func (res *ParseResult) addOCRMessage(rec *ocrRecord, provider string) {
	u := rec.Usage
	if u == nil {
		return
	}
	total := u.PromptTokens + u.CompletionTokens + u.CacheReadTokens + u.CacheWriteTokens
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

	sessID := rec.SessionID
	if sessID == "" {
		sessID = res.Session.ID
	}
	model := rec.Model

	res.Events = append(res.Events, UsageEvent{
		SessionID:  sessID,
		SourcePath: res.Session.Path,
		MsgID:      rec.UUID,
		TS:         rec.Timestamp,
		Day:        day,
		Hour:       hour,
		Project:    res.Session.Project,
		Model:      model,
		Provider:   provider,
		Kind:       res.Session.Kind,
		Source:     SourceOCR,
		Input:      u.PromptTokens,
		Output:     u.CompletionTokens,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheWriteTokens,
		Total:      total,
	})
}

// seedOCRHeader recovers session identity from the top of an already-open ocr
// transcript without emitting events. The caller re-seeks afterwards.
func seedOCRHeader(f *os.File, res *ParseResult) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(io.LimitReader(f, headerScanMaxBytes))
	for i := 0; i < headerScanMaxLines; i++ {
		line, err := r.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var rec ocrRecord
			if json.Unmarshal([]byte(trimmed), &rec) == nil && rec.Type == "session_start" {
				res.consumeOCR(&rec, "")
				if res.Session.ID != "" && res.Session.CWD != "" {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// OCRProviderName reads the active provider name from ocr's config.json, which
// sits next to the sessions directory (~/.opencodereview/config.json). Only
// the top-level "provider" key is read - the file also holds API keys and they
// must never be touched. Returns "" when absent or unreadable.
func OCRProviderName(sessionsRoot string) string {
	clean := filepath.Clean(sessionsRoot)
	cfgPath := filepath.Join(filepath.Dir(clean), "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Provider)
}
