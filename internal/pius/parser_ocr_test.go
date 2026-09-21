package pius

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const ocrSessHdr = `{"type":"session_start","sessionId":"ocr-s1","uuid":"u-start","cwd":"/home/u/projects/widget","timestamp":"2026-01-02T09:00:00Z","model":"m-review"}`

const ocrResp1 = `{"type":"llm_response","sessionId":"ocr-s1","uuid":"u-r1","model":"m-review","taskType":"plan_task","timestamp":"2026-01-02T10:30:00Z","usage":{"prompt_tokens":100,"completion_tokens":40,"cache_read_tokens":500,"cache_write_tokens":7}}`

const ocrResp2 = `{"type":"llm_response","sessionId":"ocr-s1","uuid":"u-r2","model":"m-review","taskType":"main_task","timestamp":"2026-01-02T11:15:00Z","usage":{"prompt_tokens":10,"completion_tokens":5,"cache_read_tokens":0,"cache_write_tokens":0}}`

const ocrNoise = `{"type":"tool_call","sessionId":"ocr-s1","uuid":"u-t1","timestamp":"2026-01-02T10:31:00Z","tool_name":"code_search"}`

func ocrFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "464693b7-78a8-4211-865b-ddfec5c993e3.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOCRParseBasic(t *testing.T) {
	content := ocrSessHdr + "\n" + ocrNoise + "\n" + ocrResp1 + "\n" + ocrResp2 + "\n"
	p := ocrFile(t, content)
	res, err := ParseOCRFile(p, 0, time.UTC, "halogen")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("events=%d want 2 (tool_call carries no usage)", len(res.Events))
	}
	ev := res.Events[0]
	if ev.Source != SourceOCR {
		t.Fatalf("source=%s want ocr", ev.Source)
	}
	if ev.Project != "widget" {
		t.Fatalf("project=%s want widget (basename of cwd)", ev.Project)
	}
	if ev.SessionID != "ocr-s1" || ev.MsgID != "u-r1" || ev.Model != "m-review" || ev.Provider != "halogen" {
		t.Fatalf("identity: sess=%s msg=%s model=%s provider=%s", ev.SessionID, ev.MsgID, ev.Model, ev.Provider)
	}
	if ev.Input != 100 || ev.Output != 40 || ev.CacheRead != 500 || ev.CacheWrite != 7 {
		t.Fatalf("tokens: in=%d out=%d cr=%d cw=%d", ev.Input, ev.Output, ev.CacheRead, ev.CacheWrite)
	}
	if ev.Total != 647 {
		t.Fatalf("total=%d want 647 (sum of all four)", ev.Total)
	}
	if ev.Day != "2026-01-02" || ev.Hour != 10 {
		t.Fatalf("bucketing: day=%s hour=%d", ev.Day, ev.Hour)
	}
	if res.NextOffset != int64(len(content)) {
		t.Fatalf("NextOffset=%d want %d", res.NextOffset, len(content))
	}
}

func TestOCRZeroUsageAndNoHeader(t *testing.T) {
	zero := `{"type":"llm_response","sessionId":"s","uuid":"z","timestamp":"2026-01-02T10:00:00Z","usage":{"prompt_tokens":0,"completion_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0}}`
	p := ocrFile(t, ocrNoise+"\n"+zero+"\n")
	res, err := ParseOCRFile(p, 0, time.UTC, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 {
		t.Fatalf("zero-usage response must not count, got %d events", len(res.Events))
	}
	// No session_start anywhere: session id falls back at store time, project "?".
	if res.Session.ID != "" {
		t.Fatalf("session id=%q want empty", res.Session.ID)
	}
}

func TestOCRTornTailNotCommitted(t *testing.T) {
	full := ocrSessHdr + "\n" + ocrResp1 + "\n"
	torn := full + `{"type":"llm_response","sessionId":"ocr-s1","uu`
	p := ocrFile(t, torn)
	res, err := ParseOCRFile(p, 0, time.UTC, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1", len(res.Events))
	}
	if res.NextOffset != int64(len(full)) {
		t.Fatalf("NextOffset=%d want %d (torn tail must not be committed)", res.NextOffset, len(full))
	}
}

func TestOCRResumeSeedsIdentity(t *testing.T) {
	full := ocrSessHdr + "\n" + ocrResp1 + "\n"
	p := ocrFile(t, full)
	// Offset past the header: the tail pass must still know the session and project.
	off := int64(len(ocrSessHdr) + 1)
	res, err := ParseOCRFile(p, off, time.UTC, "halogen")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1", len(res.Events))
	}
	ev := res.Events[0]
	if ev.SessionID != "ocr-s1" || ev.Project != "widget" {
		t.Fatalf("tail parse lost identity: sess=%s project=%s", ev.SessionID, ev.Project)
	}
}

func TestOCRProviderName(t *testing.T) {
	root := t.TempDir()
	sess := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"provider": "halogen",
		"custom_providers": map[string]any{
			"halogen": map[string]any{"api_key": "secret-must-not-be-read"},
		},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(root, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OCRProviderName(sess); got != "halogen" {
		t.Fatalf("provider=%q want halogen", got)
	}
	if got := OCRProviderName(filepath.Join(root, "a", "b", "sessions")); got != "" {
		t.Fatalf("missing config must yield empty provider, got %q", got)
	}
}

// End-to-end: ocr files indexed through the shared cycle land in the cube with
// source='ocr' and coexist with pi files.
func TestOCRIndexIntegration(t *testing.T) {
	s := newTestStore(t)

	piDir := t.TempDir()
	piPath := filepath.Join(piDir, "2026_a.jsonl")
	if err := os.WriteFile(piPath, []byte(sessHdr+"\n"+usageMsg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ocrRoot := t.TempDir()
	repoDir := filepath.Join(ocrRoot, "home-u-projects-widget")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ocrPath := filepath.Join(repoDir, "sess-1.jsonl")
	if err := os.WriteFile(ocrPath, []byte(ocrSessHdr+"\n"+ocrResp1+"\n"+ocrResp2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Index(s, IndexOptions{Root: piDir, OCRRoot: ocrRoot})
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsAdded != 3 {
		t.Fatalf("events added=%d want 3 (1 pi + 2 ocr)", res.EventsAdded)
	}

	var n int
	s.db.QueryRow(`SELECT count(*) FROM usage_events WHERE source='ocr'`).Scan(&n)
	if n != 2 {
		t.Fatalf("ocr events=%d want 2", n)
	}
	s.db.QueryRow(`SELECT count(*) FROM usage_events WHERE source='pi'`).Scan(&n)
	if n != 1 {
		t.Fatalf("pi events=%d want 1", n)
	}

	cube, err := s.Cube()
	if err != nil {
		t.Fatal(err)
	}
	f := facetsFrom(cube)
	found := false
	for _, x := range f.Sources {
		if x == SourceOCR {
			found = true
		}
	}
	if !found {
		t.Fatalf("facets missing ocr: %v", f.Sources)
	}

	// Incremental re-run must be a no-op.
	res2, err := Index(s, IndexOptions{Root: piDir, OCRRoot: ocrRoot})
	if err != nil {
		t.Fatal(err)
	}
	if res2.EventsAdded != 0 {
		t.Fatalf("second run added=%d want 0", res2.EventsAdded)
	}

	// Appending to the ocr transcript tails only the new line.
	fh, err := os.OpenFile(ocrPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	appended := `{"type":"llm_response","sessionId":"ocr-s1","uuid":"u-r3","model":"m-review","timestamp":"2026-01-02T12:00:00Z","usage":{"prompt_tokens":1,"completion_tokens":2,"cache_read_tokens":0,"cache_write_tokens":0}}`
	fh.WriteString(appended + "\n")
	fh.Close()

	res3, err := Index(s, IndexOptions{Root: piDir, OCRRoot: ocrRoot})
	if err != nil {
		t.Fatal(err)
	}
	if res3.EventsAdded != 1 {
		t.Fatalf("tail added=%d want 1", res3.EventsAdded)
	}
}
