package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	// Embed the tz database: the scratch image has no /usr/share/zoneinfo, so
	// without this TZ=Europe/Helsinki silently fails and every day bucket lands
	// in UTC - a multi-hour shift that quietly corrupts all daily numbers.
	_ "time/tzdata"

	"github.com/gizmoz/pi-token-usage/internal/pius"
)

const (
	envSessions = "PI_SESSIONS_DIR"
	envDB       = "PITOKENS_DB"
	envAddr     = "PITOKENS_ADDR"
	envTZ       = "TZ"
	envOpenCode = "OPENCODE_DB"
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "index":
		os.Exit(cmdIndex(args))
	case "serve":
		os.Exit(cmdServe(args))
	case "report":
		os.Exit(cmdReport(args))
	case "healthcheck":
		os.Exit(cmdHealthcheck(args))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `pi-token-usage - token usage dashboard for the pi coding agent

Usage:
  pitokens <command> [flags]

Commands:
  index      Parse ~/.pi/agent/sessions into the SQLite index
               -full          wipe and re-parse everything
               -opencode PATH opencode db to ingest alongside pi
  serve      Serve the web UI and API
  report     Print text tables (no browser needed)
               -by  day | month | project | model | provider | kind | source
  healthcheck  Exit 0 if the listener answers, 1 otherwise

Environment:
  PI_SESSIONS_DIR   sessions root   (default ~/.pi/agent/sessions)
  PITOKENS_DB       database path   (default ~/.local/share/pi-token-usage/index.db)
  PITOKENS_ADDR     listen address  (default 127.0.0.1:7777)
  OPENCODE_DB       opencode db     (default ~/.local/share/opencode/opencode.db,
                                   'off' disables ingesting opencode tokens)
  TZ                day bucketing   (default system local zone)
`)
}

type config struct {
	sessions string
	db       string
	addr     string
	tz       string
	opencode string
}

func loadConfig() (config, error) {
	c := config{}
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	c.sessions = firstNonEmpty(os.Getenv(envSessions), filepath.Join(home, ".pi", "agent", "sessions"))
	c.db = firstNonEmpty(os.Getenv(envDB), filepath.Join(home, ".local", "share", "pi-token-usage", "index.db"))
	c.addr = firstNonEmpty(os.Getenv(envAddr), "127.0.0.1:7777")
	c.tz = os.Getenv(envTZ)
	c.opencode = firstNonEmpty(os.Getenv(envOpenCode), filepath.Join(home, ".local", "share", "opencode", "opencode.db"))
	// OPENCODE_DB=off opts out entirely; an unset value keeps the default path.
	if strings.EqualFold(strings.TrimSpace(c.opencode), "off") {
		c.opencode = ""
	}
	return c, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// applyZone sets day/hour bucketing. Getting this wrong silently shifts every
// daily bucket by hours, so it is resolved explicitly rather than inherited.
func applyZone(tz string) {
	loc := time.Local
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			log.Printf("cannot load TZ %q (%v); falling back to system local zone", tz, err)
		} else {
			loc = l
		}
	}
	pius.SetLocation(loc)
	log.Printf("bucketing days in %s", loc)
}

func openDB(path string) (*pius.Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	return pius.Open(path)
}

func cmdIndex(argv []string) int {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	full := fs.Bool("full", false, "wipe and re-parse everything")
	sessions := fs.String("sessions", "", "sessions root")
	dbPath := fs.String("db", "", "database path")
	openCode := fs.String("opencode", "", "opencode db path ('off' to disable)")
	_ = fs.Parse(argv)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if *sessions != "" {
		cfg.sessions = *sessions
	}
	if *dbPath != "" {
		cfg.db = *dbPath
	}
	if *openCode != "" {
		cfg.opencode = strings.TrimSpace(*openCode)
		if strings.EqualFold(cfg.opencode, "off") {
			cfg.opencode = ""
		}
	}
	applyZone(cfg.tz)

	st, err := openDB(cfg.db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	fmt.Printf("indexing %s -> %s (full=%v)\n", cfg.sessions, cfg.db, *full)
	if cfg.opencode != "" {
		fmt.Printf("opencode db:    %s\n", cfg.opencode)
	}
	res, err := pius.Index(st, pius.IndexOptions{Root: cfg.sessions, OpenCodeDB: cfg.opencode, Full: *full})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("files parsed:   %d\n", res.FilesScanned)
	fmt.Printf("files skipped:  %d (unchanged)\n", res.FilesSkipped)
	fmt.Printf("events added:   %d\n", res.EventsAdded)
	fmt.Printf("duplicates:     %d (fork copies, excluded)\n", res.EventsSkipped)
	fmt.Printf("bytes read:     %s\n", humanBytes(res.BytesRead))
	fmt.Printf("elapsed:        %d ms\n", res.ElapsedMS)

	tot, err := st.Totals()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\ntotal tokens:   %s\n", humanNum(tot.TotalTokens))
	fmt.Printf("  input         %s\n", humanNum(tot.Input))
	fmt.Printf("  output        %s\n", humanNum(tot.Output))
	fmt.Printf("  cache read    %s\n", humanNum(tot.CacheRead))
	fmt.Printf("cache hit rate: %.1f%%\n", tot.CacheHit)
	fmt.Printf("projects:       %d   days: %d\n", tot.Projects, tot.Days)
	return 0
}

func cmdServe(argv []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address")
	sessions := fs.String("sessions", "", "sessions root")
	dbPath := fs.String("db", "", "database path")
	openCode := fs.String("opencode", "", "opencode db path ('off' to disable)")
	noIndex := fs.Bool("no-index", false, "skip the startup index")
	_ = fs.Parse(argv)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if *addr != "" {
		cfg.addr = *addr
	}
	if *sessions != "" {
		cfg.sessions = *sessions
	}
	if *dbPath != "" {
		cfg.db = *dbPath
	}
	if *openCode != "" {
		cfg.opencode = strings.TrimSpace(*openCode)
		if strings.EqualFold(cfg.opencode, "off") {
			cfg.opencode = ""
		}
	}
	applyZone(cfg.tz)

	st, err := openDB(cfg.db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	if !*noIndex {
		fmt.Printf("cold index of %s ...\n", cfg.sessions)
		res, err := pius.Index(st, pius.IndexOptions{Root: cfg.sessions, OpenCodeDB: cfg.opencode})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("indexed %d files (%d unchanged) in %d ms\n",
			res.FilesScanned, res.FilesSkipped, res.ElapsedMS)
	}

	srv := pius.NewServer(st, cfg.sessions, cfg.opencode, pius.WebFS())
	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\nshutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	fmt.Printf("pi-token-usage listening on http://%s\n", cfg.addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	return 0
}

func cmdReport(argv []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	by := fs.String("by", "month", "day | month | project | model | provider | kind | source")
	dbPath := fs.String("db", "", "database path")
	_ = fs.Parse(argv)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if *dbPath != "" {
		cfg.db = *dbPath
	}
	applyZone(cfg.tz)

	st, err := openDB(cfg.db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	cube, err := st.Cube()
	if err != nil {
		log.Fatal(err)
	}
	if len(cube) == 0 {
		fmt.Println("index is empty; run: pitokens index")
		return 1
	}

	type agg struct {
		key                         string
		in, out, cache, total, msgs int
	}
	buckets := map[string]*agg{}
	for _, c := range cube {
		k := c.Day
		switch *by {
		case "month":
			if len(c.Day) >= 7 {
				k = c.Day[:7]
			}
		case "project":
			k = c.Project
		case "model":
			k = c.Model
		case "provider":
			k = c.Provider
		case "kind":
			k = string(c.Kind)
		case "source":
			k = c.Source
		}
		b := buckets[k]
		if b == nil {
			b = &agg{key: k}
			buckets[k] = b
		}
		b.in += c.Input
		b.out += c.Output
		b.cache += c.CacheRead
		b.total += c.Total
		b.msgs += c.Msgs
	}

	rows := make([]*agg, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, b)
	}
	sort.Slice(rows, func(i, j int) bool {
		if *by == "day" || *by == "month" {
			return rows[i].key < rows[j].key
		}
		return rows[i].total > rows[j].total
	})

	fmt.Printf("\n%-26s %14s %14s %14s %8s\n", strings.ToUpper(*by), "INPUT", "OUTPUT", "TOTAL", "MSGS")
	line := strings.Repeat("-", 26+14*3+8+4)
	fmt.Println(line)
	var tIn, tOut, tTot, tMsg int
	for _, b := range rows {
		k := b.key
		if len(k) > 26 {
			k = k[:26]
		}
		fmt.Printf("%-26s %14s %14s %14s %8d\n", k, humanNum(b.in), humanNum(b.out), humanNum(b.total), b.msgs)
		tIn += b.in
		tOut += b.out
		tTot += b.total
		tMsg += b.msgs
	}
	fmt.Println(line)
	fmt.Printf("%-26s %14s %14s %14s %8d\n", "TOTAL", humanNum(tIn), humanNum(tOut), humanNum(tTot), tMsg)
	return 0
}

func cmdHealthcheck(argv []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address")
	_ = fs.Parse(argv)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *addr != "" {
		cfg.addr = *addr
	}
	if err := pius.Healthcheck(cfg.addr, 3*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	fmt.Println("healthy")
	return 0
}

func humanNum(n int) string {
	if n == 0 {
		return "0"
	}
	s := fmt.Sprintf("%d", n)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, " ")
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
