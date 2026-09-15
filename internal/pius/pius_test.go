package pius

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tmpFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T10-00-00-000Z_test.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sessHdr = `{"type":"session","version":3,"id":"s1","timestamp":"2026-01-02T10:00:00.000Z","cwd":"/home/u/proj"}`
const usageMsg = `{"type":"message","id":"m1","parentId":"p","timestamp":"2026-01-02T10:00:05.000Z","message":{"role":"assistant","model":"M","provider":"P","usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":0,"totalTokens":60}}}`

// ---------------------------------------------------------------- offsets

func TestOffsetExactWithTrailingNewline(t *testing.T) {
	content := sessHdr + "\n" + usageMsg + "\n"
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if res.NextOffset != int64(len(content)) {
		t.Fatalf("NextOffset=%d want %d (must land exactly at EOF)", res.NextOffset, len(content))
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1", len(res.Events))
	}
}

func TestOffsetExactWithoutTrailingNewline(t *testing.T) {
	content := sessHdr + "\n" + usageMsg // no trailing \n
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// The last line has no terminator, so it is still parsed but the offset must
	// not claim bytes we cannot re-find on a later pass.
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1", len(res.Events))
	}
	if res.NextOffset != int64(len(content)) {
		t.Fatalf("NextOffset=%d want %d", res.NextOffset, len(content))
	}
}

func TestIncompleteTailIsNotCommitted(t *testing.T) {
	// Simulate pi writing a record while we read: the tail is cut off.
	full := sessHdr + "\n" + usageMsg + "\n"
	torn := full + `{"type":"message","id":"m2","parentId":"p","timestamp":"2026-01`
	p := tmpFile(t, torn)

	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1 (torn tail must not count)", len(res.Events))
	}
	if res.NextOffset != int64(len(full)) {
		t.Fatalf("NextOffset=%d want %d (stop before torn bytes)", res.NextOffset, len(full))
	}

	// Now complete the write; resuming from the stored offset must pick it up.
	if err := os.WriteFile(p, []byte(torn+`0:00:05.000Z","message":{"role":"assistant","model":"M","provider":"P","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := ParseFile(p, res.NextOffset, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Events) != 1 {
		t.Fatalf("after completing write: events=%d want 1", len(res2.Events))
	}
	if res2.Events[0].MsgID != "m2" {
		t.Fatalf("got msg %q want m2", res2.Events[0].MsgID)
	}
}

func TestMalformedCompleteLineIsSkippedNotFatal(t *testing.T) {
	// A corrupt line in the MIDDLE must not stop the file from indexing.
	content := sessHdr + "\n" + "{this is not json}\n" + usageMsg + "\n"
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if res.Malformed != 1 {
		t.Fatalf("malformed=%d want 1", res.Malformed)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1 (later records must still be read)", len(res.Events))
	}
	if res.NextOffset != int64(len(content)) {
		t.Fatalf("NextOffset=%d want %d (must reach EOF past the bad line)", res.NextOffset, len(content))
	}
}

func TestEmptyLinesAndBlankTail(t *testing.T) {
	content := sessHdr + "\n\n\n" + usageMsg + "\n\n"
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1", len(res.Events))
	}
	if res.NextOffset != int64(len(content)) {
		t.Fatalf("NextOffset=%d want %d", res.NextOffset, len(content))
	}
}

// The regression this whole test file exists for: counting only the chunk length
// (bufio.ReadLine strips the newline) drifts the offset by 1 byte per line.
func TestOffsetDoesNotDriftAcrossManyLines(t *testing.T) {
	var b strings.Builder
	b.WriteString(sessHdr + "\n")
	for i := 0; i < 500; i++ {
		b.WriteString(usageMsg + "\n")
	}
	content := b.String()
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if res.NextOffset != int64(len(content)) {
		t.Fatalf("offset drifted: got %d want %d (delta %d bytes)",
			res.NextOffset, len(content), int64(len(content))-res.NextOffset)
	}
	if len(res.Events) != 500 {
		t.Fatalf("events=%d want 500", len(res.Events))
	}
}

func TestResumeFromOffsetReadsOnlyNewBytes(t *testing.T) {
	first := sessHdr + "\n" + usageMsg + "\n"
	p := tmpFile(t, first)
	r1, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	second := `{"type":"message","id":"m9","parentId":"p","timestamp":"2026-01-02T11:00:00.000Z","message":{"role":"assistant","model":"M","provider":"P","usage":{"input":5,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":10}}}` + "\n"
	if err := os.WriteFile(p, []byte(first+second), 0o644); err != nil {
		t.Fatal(err)
	}
	r2, err := ParseFile(p, r1.NextOffset, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Events) != 1 || r2.Events[0].MsgID != "m9" {
		t.Fatalf("resume events=%v want just m9", r2.Events)
	}
	if r2.BytesScanned != int64(len(second)) {
		t.Fatalf("bytes scanned=%d want %d (tail only)", r2.BytesScanned, len(second))
	}
}

func TestOffsetAtOrPastEOF(t *testing.T) {
	content := sessHdr + "\n"
	p := tmpFile(t, content)
	res, err := ParseFile(p, int64(len(content)), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 {
		t.Fatal("expected no events past EOF")
	}
	if res.NextOffset != int64(len(content)) {
		t.Fatalf("NextOffset should be unchanged, got %d", res.NextOffset)
	}
}

// ---------------------------------------------------------------- timezone

func TestDayBucketingUsesLocalZone(t *testing.T) {
	// 23:00 UTC on Jan 2 is Jan 3 in a UTC+1 zone.
	plus1 := time.FixedZone("UTC+1", 3600)
	content := sessHdr + "\n" +
		`{"type":"message","id":"late","parentId":"p","timestamp":"2026-01-02T23:30:00.000Z","message":{"role":"assistant","model":"M","provider":"P","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}}` + "\n"
	p := tmpFile(t, content)

	utcRes, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if utcRes.Events[0].Day != "2026-01-02" {
		t.Fatalf("UTC day=%s want 2026-01-02", utcRes.Events[0].Day)
	}

	locRes, err := ParseFile(p, 0, plus1)
	if err != nil {
		t.Fatal(err)
	}
	if locRes.Events[0].Day != "2026-01-03" {
		t.Fatalf("local day=%s want 2026-01-03 (late-night work must roll over)", locRes.Events[0].Day)
	}
	if locRes.Events[0].Hour != 0 {
		t.Fatalf("local hour=%d want 0", locRes.Events[0].Hour)
	}
}

// ---------------------------------------------------------------- classify

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"/s/--proj--/2026_x.jsonl":                              KindMain,
		"/s/--proj--/stem/forks/2026_fork.jsonl":                KindFork,
		"/s/--proj--/stem/abc/run-0/session.jsonl":              KindSubagent,
		"/s/--proj--/subagent-artifacts/x_worker_transcript.jsonl": KindMain, // ignored separately
	}
	for path, want := range cases {
		if got := classify(path); got != want {
			t.Errorf("classify(%s)=%s want %s", path, got, want)
		}
	}
}

func TestIsIgnoredArtifacts(t *testing.T) {
	if !IsIgnored("/s/--p--/subagent-artifacts/x_transcript.jsonl") {
		t.Error("artifact transcript must be ignored")
	}
	if IsIgnored("/s/--p--/2026_x.jsonl") {
		t.Error("normal session must not be ignored")
	}
}

// ---------------------------------------------------------------- dedupe

func newTestStore(t *testing.T) *Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func indexOnce(t *testing.T, s *Store, path string) *ParseResult {
	t.Helper()
	res, err := ParseFile(path, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreFile(tx, res, st.Size(), st.ModTime().UnixNano()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return res
}

// A fork that byte-for-byte copies parent records must contribute nothing new.
func TestForkCopyIsDeduped(t *testing.T) {
	s := newTestStore(t)
	mainDir := t.TempDir()
	mainPath := filepath.Join(mainDir, "2026_a.jsonl")
	os.WriteFile(mainPath, []byte(sessHdr+"\n"+usageMsg+"\n"), 0o644)
	indexOnce(t, s, mainPath)

	forkHdr := `{"type":"session","version":3,"id":"s2","timestamp":"2026-01-02T11:00:00.000Z","cwd":"/home/u/proj"}`
	forkPath := filepath.Join(mainDir, "stem", "forks", "2026_f.jsonl")
	os.MkdirAll(filepath.Dir(forkPath), 0o755)
	os.WriteFile(forkPath, []byte(forkHdr+"\n"+usageMsg+"\n"), 0o644)
	res := indexOnce(t, s, forkPath)

	if res.Skipped != 1 {
		t.Fatalf("fork copy skipped=%d want 1", res.Skipped)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&n)
	if n != 1 {
		t.Fatalf("total events=%d want 1 (fork copy must not double-count)", n)
	}
}

// Re-parsing the SAME file must not dedupe it against itself.
func TestReparseSameFileIsNotSelfDeduped(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "2026_a.jsonl")
	os.WriteFile(p, []byte(sessHdr+"\n"+usageMsg+"\n"), 0o644)
	indexOnce(t, s, p)

	// Simulate a full re-parse (compaction detected): reset then re-index.
	tx, _ := s.db.Begin()
	if err := s.ResetFile(tx, p); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()

	res := indexOnce(t, s, p)
	if res.Skipped != 0 {
		t.Fatalf("self-deduped: skipped=%d want 0", res.Skipped)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&n)
	if n != 1 {
		t.Fatalf("events=%d want 1", n)
	}
}

// Same message id but a different token count is a different event, not a copy.
func TestDedupeKeyIncludesTokenCount(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "2026_a.jsonl")
	os.WriteFile(a, []byte(sessHdr+"\n"+usageMsg+"\n"), 0o644)
	indexOnce(t, s, a)

	hdrB := `{"type":"session","version":3,"id":"s2","timestamp":"2026-01-02T12:00:00.000Z","cwd":"/home/u/other"}`
	b := filepath.Join(dir, "2026_b.jsonl")
	os.WriteFile(b, []byte(hdrB+"\n"+strings.Replace(usageMsg, `"totalTokens":60`, `"totalTokens":999`, 1)+"\n"), 0o644)
	res := indexOnce(t, s, b)

	if res.Skipped != 0 {
		t.Fatalf("skipped=%d want 0 (different token count is not a copy)", res.Skipped)
	}
}

// ---------------------------------------------------------------- rollup

func TestFactsRebuildIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "2026_a.jsonl")
	os.WriteFile(p, []byte(sessHdr+"\n"+usageMsg+"\n"), 0o644)
	indexOnce(t, s, p)

	if err := s.RebuildFacts(); err != nil {
		t.Fatal(err)
	}
	first := queryTotals(t, s)

	// Rebuilding again from the same events must produce identical numbers.
	if err := s.RebuildFacts(); err != nil {
		t.Fatal(err)
	}
	second := queryTotals(t, s)

	if first != second {
		t.Fatalf("rollup drifted: %v then %v", first, second)
	}
	if first.total != 60 {
		t.Fatalf("total=%d want 60", first.total)
	}
}

type factTotals struct{ total, input, output, cache, msgs int }

func queryTotals(t *testing.T, s *Store) factTotals {
	t.Helper()
	var f factTotals
	err := s.db.QueryRow(`SELECT COALESCE(sum(total),0),COALESCE(sum(input),0),
		COALESCE(sum(output),0),COALESCE(sum(cache_read),0),COALESCE(sum(msgs),0) FROM facts`).
		Scan(&f.total, &f.input, &f.output, &f.cache, &f.msgs)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTotalTokensFallbackWhenAbsent(t *testing.T) {
	content := sessHdr + "\n" +
		`{"type":"message","id":"nt","parentId":"p","timestamp":"2026-01-02T10:00:00.000Z","message":{"role":"assistant","model":"M","provider":"P","usage":{"input":7,"output":3,"cacheRead":5,"cacheWrite":0}}}` + "\n"
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1", len(res.Events))
	}
	if got := res.Events[0].Total; got != 15 {
		t.Fatalf("derived total=%d want 15 (input+output+cacheRead+cacheWrite)", got)
	}
}

func TestZeroTokenMessageIsNotCounted(t *testing.T) {
	content := sessHdr + "\n" +
		`{"type":"message","id":"z","parentId":"p","timestamp":"2026-01-02T10:00:00.000Z","message":{"role":"assistant","model":"M","provider":"P","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0}}}` + "\n"
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 {
		t.Fatalf("events=%d want 0", len(res.Events))
	}
}

func TestNonAssistantRolesIgnored(t *testing.T) {
	content := sessHdr + "\n" +
		`{"type":"message","id":"u","parentId":"p","timestamp":"2026-01-02T10:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n" +
		`{"type":"message","id":"tr","parentId":"p","timestamp":"2026-01-02T10:00:01.000Z","message":{"role":"toolResult"}}` + "\n"
	p := tmpFile(t, content)
	res, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 {
		t.Fatalf("events=%d want 0 (only assistant messages count)", len(res.Events))
	}
}

func TestProjectFromCWD(t *testing.T) {
	cases := map[string]string{
		"/home/u/Projects/foo": "foo",
		"/home/u":              "u",
		"":                     "?",
		"/":                    "/",
	}
	for in, want := range cases {
		if got := projectFromCWD(in); got != want {
			t.Errorf("projectFromCWD(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseTSVariants(t *testing.T) {
	for _, s := range []string{
		"2026-01-02T10:00:00.000Z",
		"2026-01-02T10:00:00Z",
		"2026-01-02T10:00:00.123456789Z",
		"2026-01-02T12:00:00+02:00",
	} {
		if parseTS(s).IsZero() {
			t.Errorf("parseTS(%q) returned zero", s)
		}
	}
	if !parseTS("").IsZero() {
		t.Error("empty timestamp should be zero")
	}
	if !parseTS("not a time").IsZero() {
		t.Error("garbage timestamp should be zero, not panic")
	}
}

// ---------------------------------------------------------------- resume identity

const sessInfo = `{"type":"session_info","name":"worker"}`

var usageMsg2 = strings.Replace(usageMsg, `"id":"m1"`, `"id":"m2"`, 1)

// A resumed (tail) parse starts *after* the `session` header record, so identity
// has to be recovered from the top of the file. Without that, every tailed event
// lands with an empty session_id and project - the UI's "(unknown)" bucket.
func TestResumeSeedsProjectAndSessionID(t *testing.T) {
	content := sessHdr + "\n" + sessInfo + "\n" + usageMsg + "\n"
	p := tmpFile(t, content)

	cold, err := ParseFile(p, 0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if cold.Session.Project != "proj" || cold.Session.ID != "s1" {
		t.Fatalf("cold parse identity: project=%q id=%q", cold.Session.Project, cold.Session.ID)
	}

	// Resume with the header behind us.
	res, err := ParseFile(p, int64(len(sessHdr)+1), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1", len(res.Events))
	}
	ev := res.Events[0]
	if ev.Project != "proj" {
		t.Fatalf("tail-parsed project=%q want %q (empty renders as unknown)", ev.Project, "proj")
	}
	if ev.SessionID != "s1" {
		t.Fatalf("tail-parsed session_id=%q want %q", ev.SessionID, "s1")
	}
	if res.Session.Agent != "worker" {
		t.Fatalf("agent=%q want %q", res.Session.Agent, "worker")
	}
}

// Resuming deep into the file (the common case: a long session keeps growing) must
// still carry identity, and must not re-read or double-count earlier records.
func TestResumeDeepInFileKeepsIdentityWithoutRereading(t *testing.T) {
	content := sessHdr + "\n" + sessInfo + "\n" + usageMsg + "\n" + usageMsg2 + "\n"
	p := tmpFile(t, content)
	off := int64(len(sessHdr) + 1 + len(sessInfo) + 1 + len(usageMsg) + 1)

	res, err := ParseFile(p, off, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events=%d want 1 (only bytes past the offset may be counted)", len(res.Events))
	}
	if res.Events[0].MsgID != "m2" {
		t.Fatalf("msg_id=%q want m2", res.Events[0].MsgID)
	}
	if res.Events[0].Project != "proj" || res.Events[0].SessionID != "s1" {
		t.Fatalf("identity lost on deep resume: project=%q session_id=%q",
			res.Events[0].Project, res.Events[0].SessionID)
	}
	if res.BytesScanned > int64(len(usageMsg2)+1) {
		t.Fatalf("bytes scanned=%d, must not re-read the header region", res.BytesScanned)
	}
}

// A file with no session header at all must not make the resume scan walk the
// whole file looking for one.
func TestHeaderScanIsBoundedWhenNoHeaderExists(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString(usageMsg2 + "\n")
	}
	p := tmpFile(t, b.String())

	res, err := ParseFile(p, int64(len(usageMsg2)+1), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesScanned > int64(200*(len(usageMsg2)+1)) {
		t.Fatalf("bytes scanned=%d exceeds file content", res.BytesScanned)
	}
	if got := len(res.Events); got != 199 {
		t.Fatalf("events=%d want 199", got)
	}
}

// ---------------------------------------------------------------- self-healing

// The pre-fix parser wrote tail events with empty session_id/project. Reopening
// the database must drop those rows *and their resume offsets* so the next index
// pass re-reads the files from byte 0.
func TestRepairUnknownEventsForcesReparse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	const src = "/sessions/--home-u-proj--/a.jsonl"
	mustExec(t, s, `INSERT INTO usage_events (session_id, source_path, msg_id, day, project, kind, total)
		VALUES ('', ?, 'm9', '2026-01-02', '', 'main', 99)`, src)
	mustExec(t, s, `INSERT INTO seen_msg (msg_id, tok, source_path) VALUES ('m9', 99, ?)`, src)
	mustExec(t, s, `INSERT INTO files (path, size, mtime_ns, offset, indexed_at) VALUES (?, 100, 1, 100, 'x')`, src)
	mustExec(t, s, `INSERT INTO sessions (id, cwd, project, kind) VALUES (?, '', '', 'main')`, "path:"+src)
	// Pretend this database predates the fix.
	mustExec(t, s, `DELETE FROM meta WHERE key = ?`, repairUnknownKey)
	s.Close()

	repaired, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()

	if n := countRows(t, repaired, "usage_events"); n != 0 {
		t.Fatalf("unknown events after repair=%d want 0", n)
	}
	if n := countRows(t, repaired, "seen_msg"); n != 0 {
		t.Fatalf("stale dedupe keys after repair=%d want 0", n)
	}
	if n := countRows(t, repaired, "files"); n != 0 {
		t.Fatalf("resume offsets survived repair=%d want 0 (file must be re-parsed)", n)
	}
	if n := countRows(t, repaired, "sessions"); n != 0 {
		t.Fatalf("synthetic session rows after repair=%d want 0", n)
	}
	if repaired.GetMeta(repairUnknownKey) == "" {
		t.Fatal("repair flag not recorded; migration would rerun on every open")
	}

	// Idempotent: a third open must be a no-op, not an error.
	again, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if n := countRows(t, again, "usage_events"); n != 0 {
		t.Fatalf("events after second repair=%d want 0", n)
	}
}

func mustExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// The scratch image ships no writable /tmp, and a restarted container's tmpfs is
// not usable by uid 65534. The facts rollup is an INSERT...SELECT with a GROUP
// BY, which with temp_store=FILE needs a temp file and dies with
// "disk I/O error (6410)" (SQLITE_IOERR_ROLLBACK_ATOMIC). Pin it to MEMORY.
func TestTempStoreIsMemory(t *testing.T) {
	s := newTestStore(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA temp_store`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 2 { // 0=DEFAULT 1=FILE 2=MEMORY
		t.Fatalf("temp_store=%d want 2 (MEMORY)", v)
	}
}
