// Command mcpkg is the whole system in one binary:
//
//	mcpkg migrate                     apply database migrations
//	mcpkg discover -source registry   crawl sources into the catalog
//	mcpkg worker -stages health,...   run pipeline hooks from the queue
//	mcpkg scheduler                   periodic discovery, re-checks, lease reaping
//	mcpkg serve                       website + JSON API + subregistry API
//	mcpkg enqueue -stage health -all  batch-add tools to a stage
//	mcpkg run -slug <slug>            run the full pipeline for one tool, synchronously
//	mcpkg add <url>                   register a repo / package / endpoint
//	mcpkg stats                       catalog and queue numbers
//	mcpkg finder -api <url>           stdio MCP server for agents
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mcpkg/mcpkg/internal/app"
	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/db"
	"github.com/mcpkg/mcpkg/internal/finder"
	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/llm"
	"github.com/mcpkg/mcpkg/internal/logx"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/stages"
	"github.com/mcpkg/mcpkg/internal/store"
	"github.com/mcpkg/mcpkg/internal/web"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfg := config.Load()
	log := newLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "migrate":
		err = cmdMigrate(ctx, cfg, log)
	case "discover":
		err = cmdDiscover(ctx, cfg, log, args)
	case "worker":
		err = cmdWorker(ctx, cfg, log, args)
	case "scheduler":
		err = cmdScheduler(ctx, cfg, log, args)
	case "serve":
		err = cmdServe(ctx, cfg, log, args)
	case "enqueue":
		err = cmdEnqueue(ctx, cfg, log, args)
	case "run":
		err = cmdRun(ctx, cfg, log, args)
	case "add":
		err = cmdAdd(ctx, cfg, log, args)
	case "stats":
		err = cmdStats(ctx, cfg, log)
	case "finder":
		err = cmdFinder(ctx, cfg, args)
	case "eval-llm":
		err = cmdEvalLLM(ctx, cfg, log, args)
	case "version", "-v", "--version":
		fmt.Println("mcpkg", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mcpkg: catalog, test and serve open-source MCP servers

Usage: mcpkg <command> [flags]

  migrate                         apply database migrations
  discover  -source registry,npm,pypi,crates,github [-limit N] [-full]
  worker    [-stages health,security,smoke,enrich,render]
  scheduler                       periodic discovery + re-checks (run one per deployment)
  serve     [-addr :8080]         website, JSON API, /v0.1/servers, badges
  enqueue   -stage health (-all | -slug a,b | -status S | -smoke-status S | -min-stars N | -never-checked | -stale-days N) [-limit N]
  run       -slug S [-from stage] run every stage for one tool now and print results
  add       <url>                 register a GitHub repo, npm/PyPI page or https MCP endpoint
  stats                           catalog + queue numbers
  finder    -api https://site     stdio MCP server exposing search_tools / get_install_instructions
  eval-llm  [-n 50] [-price-in X -price-out Y]  measure an LLM's pass rate + cost before a full run

Configuration comes from environment variables; see .env.example.
`)
}

func newLogger(level string) *slog.Logger { return logx.New(level, "mcpkg") }

func open(ctx context.Context, cfg config.Config, log *slog.Logger) (*app.App, error) {
	return app.Open(ctx, cfg, log)
}

func cmdMigrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	applied, err := db.Migrate(ctx, a.DB)
	for _, m := range applied {
		log.Info("applied migration", "file", m)
	}
	if err == nil && len(applied) == 0 {
		log.Info("database is up to date")
	}
	return err
}

func cmdDiscover(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	src := fs.String("source", "registry", "comma-separated: registry,npm,pypi,crates,github")
	limit := fs.Int("limit", 0, "stop after N candidates per source (0 = all; limited runs don't move the cursor)")
	full := fs.Bool("full", false, "ignore the incremental cursor")
	_ = fs.Parse(args)
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	srcs, err := a.Sources(strings.Split(*src, ","))
	if err != nil {
		return err
	}
	d := a.Discoverer()
	for _, s := range srcs {
		start := time.Now()
		st, err := d.Run(ctx, s, *limit, *full)
		if err == nil && *limit == 0 {
			// A complete manual crawl counts as the scheduler's run for this source.
			_ = a.Store.SetCursor(ctx, "lastrun:"+s.Name(), start.UTC().Format(time.RFC3339))
		}
		log.Info("discovery done", "source", st.Source, "seen", st.Seen, "created", st.Created, "updated", st.Updated,
			"merged", st.Merged, "enqueued", st.Enqueued, "errors", st.Errors, "took", time.Since(start).Round(time.Second))
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name(), err)
		}
	}
	return nil
}

func cmdWorker(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	names := fs.String("stages", "health,security,smoke,enrich,render", "stages this worker runs")
	_ = fs.Parse(args)
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	r := a.Runner()
	log.Info("worker started", "id", cfg.WorkerID, "stages", *names, "sandbox", cfg.Sandbox, "llm", cfg.LLMBaseURL != "")
	return r.Work(ctx, strings.Split(*names, ","))
}

// schedule of periodic discovery.
var schedule = []struct {
	source string
	every  time.Duration
}{
	{"registry", time.Hour}, // the registry asks aggregators to poll about hourly
	{"npm", 24 * time.Hour},
	{"pypi", 24 * time.Hour},
	{"github", 24 * time.Hour},
	{"crates", 7 * 24 * time.Hour},
}

func cmdScheduler(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("scheduler", flag.ExitOnError)
	only := fs.String("sources", "registry,npm,pypi,github,crates", "sources to crawl periodically")
	_ = fs.Parse(args)
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	enabled := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		enabled[strings.TrimSpace(s)] = true
	}
	if cfg.GitHubToken == "" {
		enabled["github"] = false
	}
	d := a.Discoverer()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	lastHousekeeping := time.Time{}
	var mu sync.Mutex
	running := map[string]bool{}
	log.Info("scheduler started", "sources", *only)
	for {
		for _, s := range schedule {
			if !enabled[s.source] {
				continue
			}
			mu.Lock()
			busy := running[s.source]
			mu.Unlock()
			if busy {
				continue
			}
			key := "lastrun:" + s.source
			last, _ := a.Store.GetCursor(ctx, key)
			if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < s.every {
				continue
			}
			// Each source crawls in its own goroutine, so a slow PyPI scan never delays the hourly registry poll.
			mu.Lock()
			running[s.source] = true
			mu.Unlock()
			go func(source string) {
				defer func() { mu.Lock(); running[source] = false; mu.Unlock() }()
				srcs, _ := a.Sources([]string{source})
				start := time.Now()
				st, err := d.Run(ctx, srcs[0], 0, false)
				switch {
				case ctx.Err() != nil:
					return // shutting down: do not record a partial run
				case err != nil:
					// Record the attempt anyway so a broken source retries at its normal interval, not every minute.
					log.Warn("scheduled discovery failed", "source", source, "err", err)
				default:
					log.Info("scheduled discovery", "source", source, "seen", st.Seen, "created", st.Created, "enqueued", st.Enqueued, "took", time.Since(start).Round(time.Second))
				}
				_ = a.Store.SetCursor(context.Background(), "lastrun:"+source, start.UTC().Format(time.RFC3339))
			}(s.source)
		}
		if time.Since(lastHousekeeping) > 10*time.Minute {
			lastHousekeeping = time.Now()
			if n, err := a.Queue.ReapExpired(ctx); err == nil && n > 0 {
				log.Warn("requeued expired leases", "jobs", n)
			}
			if n, err := a.Queue.Cleanup(ctx, 7*24*time.Hour); err == nil && n > 0 {
				log.Info("cleaned finished jobs", "jobs", n)
			}
			// Self-healing: tools whose scheduled re-check got lost are queued again (at most once per window).
			if days := a.Pipe.Recheck.HealthEveryDays; days > 0 {
				if ids, err := a.Store.SelectStaleIDs(ctx, days*2, 5000); err == nil && len(ids) > 0 {
					if n, _ := a.Queue.EnqueueMany(ctx, a.Pipe.Entry, ids, time.Time{}); n > 0 {
						log.Info("queued stale tools for re-check", "tools", n)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func cmdServe(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", cfg.Addr, "listen address")
	migrate := fs.Bool("migrate", true, "apply migrations on start")
	_ = fs.Parse(args)
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	if *migrate {
		if _, err := db.Migrate(ctx, a.DB); err != nil {
			return err
		}
	}
	res := a.Resolver()
	srv, err := web.New(cfg, a.Store, res, log)
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("serving", "addr", *addr, "site", cfg.SiteURL)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdEnqueue(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("enqueue", flag.ExitOnError)
	stage := fs.String("stage", "health", "stage to enqueue")
	all := fs.Bool("all", false, "every tool")
	slugs := fs.String("slug", "", "comma-separated slugs")
	status := fs.String("status", "", "tool status filter")
	smoke := fs.String("smoke-status", "", "smoke status filter")
	minStars := fs.Int("min-stars", 0, "minimum stars")
	never := fs.Bool("never-checked", false, "only tools never verified")
	stale := fs.Int("stale-days", 0, "only tools not verified for N days")
	limit := fs.Int("limit", 0, "max tools")
	_ = fs.Parse(args)
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	if _, ok := a.Pipe.Stages[*stage]; !ok {
		return fmt.Errorf("unknown stage %q", *stage)
	}
	f := store.Filter{All: *all, Status: *status, SmokeStatus: *smoke, MinStars: *minStars, NeverChecked: *never, StaleDays: *stale, Limit: *limit}
	if *slugs != "" {
		f.Slugs = strings.Split(*slugs, ",")
	}
	ids, err := a.Store.SelectIDs(ctx, f)
	if err != nil {
		return err
	}
	n, err := a.Queue.EnqueueMany(ctx, *stage, ids, time.Time{})
	log.Info("enqueued", "stage", *stage, "matched", len(ids), "new_jobs", n)
	return err
}

func cmdRun(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	slug := fs.String("slug", "", "tool slug")
	from := fs.String("from", "", "start at this stage (default: entry)")
	_ = fs.Parse(args)
	if *slug == "" {
		return errors.New("-slug is required")
	}
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	t, err := a.Store.GetToolBySlug(ctx, *slug)
	if err != nil {
		return err
	}
	r := a.Runner()
	return r.RunSync(ctx, t.ID, *from, func(stage string, res pipeline.Result) {
		fmt.Printf("== %-8s decision=%s", stage, orDash(string(res.Decision)))
		if res.Err != nil {
			fmt.Printf(" error=%v", res.Err)
		}
		fmt.Println()
		for _, n := range res.Notes {
			fmt.Println("   note:", n)
		}
		if res.Review != "" {
			fmt.Println("   review:", res.Review)
		}
		if v, ok := res.Fields[stage]; ok {
			b, _ := json.MarshalIndent(v, "   ", "  ")
			if len(b) > 4000 {
				b = append(b[:4000], []byte("\n   …")...)
			}
			fmt.Println("  ", string(b))
		}
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdAdd(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mcpkg add <url>")
	}
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	res := a.Resolver()
	r, c, err := res.Submit(ctx, args[0])
	if err != nil {
		return err
	}
	t, err := a.Store.GetTool(ctx, r.ToolID)
	if err != nil {
		return err
	}
	_ = a.Store.RecordSubmission(ctx, args[0], "cli", r.ToolID, "accepted", "")
	fmt.Printf("added %s (source=%s, slug=%s, created=%v). Queued for checks.\n", t.Name, c.Source, t.Slug, r.Created)
	return nil
}

func cmdStats(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	st, err := a.Store.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("tools: %d total, %d listed, %d verified, %d pending\n", st.Total, st.Listed, st.Verified, st.Pending)
	fmt.Printf("smoke: %d works, %d need key/auth, %d broken; %d blocked, %d unmaintained\n", st.Works, st.NeedsKey, st.Broken, st.Blocked, st.Unmaintained)
	fmt.Printf("by source: %v\n", st.BySource)
	qs, err := a.Queue.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\n%-10s %8s %8s %8s %8s %8s\n", "stage", "queued", "due", "running", "dead", "done")
	for _, s := range qs {
		fmt.Printf("%-10s %8d %8d %8d %8d %8d\n", s.Stage, s.Queued, s.Due, s.Running, s.Dead, s.Done)
	}
	return nil
}

func cmdFinder(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("finder", flag.ExitOnError)
	api := fs.String("api", cfg.SiteURL, "base URL of the catalog")
	_ = fs.Parse(args)
	s := &finder.Server{API: strings.TrimRight(*api, "/"), Name: cfg.SiteName, Version: version, HTTP: httpx.New(cfg.UserAgent)}
	return s.Serve(ctx, os.Stdin, os.Stdout)
}

// cmdEvalLLM runs the enrich step on a random sample without saving, so you can
// compare open models (quality + cost) before spending money on 50k tools.
func cmdEvalLLM(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("eval-llm", flag.ExitOnError)
	n := fs.Int("n", 50, "sample size (tools that have a README)")
	priceIn := fs.Float64("price-in", 0, "USD per 1M input tokens, for the cost estimate")
	priceOut := fs.Float64("price-out", 0, "USD per 1M output tokens, for the cost estimate")
	catalog := fs.Int("catalog", 50000, "catalog size to extrapolate the cost to")
	show := fs.Int("show", 5, "print this many sample outputs")
	_ = fs.Parse(args)
	if cfg.LLMBaseURL == "" {
		return errors.New("set LLM_BASE_URL and LLM_MODEL (any OpenAI-compatible endpoint)")
	}
	a, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(ctx, `SELECT id FROM tools WHERE readme <> '' ORDER BY random() LIMIT $1`, *n)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	tools, err := a.Store.GetTools(ctx, ids)
	if err != nil {
		return err
	}
	e := &stages.Enrich{D: &stages.Deps{Cfg: cfg, HTTP: a.HTTP, Store: a.Store, Pipe: a.Pipe, Log: log},
		LLM: &llm.Client{BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel, JSONMode: cfg.LLMJSONMode, HTTP: a.HTTP}}
	var pass, firstTry, inTok, outTok int
	var total time.Duration
	reasons := map[string]int{}
	for i, t := range tools {
		start := time.Now()
		out, rejected, _, err := e.Generate(ctx, t)
		total += time.Since(start)
		if err != nil {
			return err
		}
		if out.Source == "llm" {
			pass++
			if len(rejected) == 0 {
				firstTry++
			}
			inTok += out.InputTokens
			outTok += out.OutputTokens
		}
		for _, r := range rejected {
			key := r
			if i := strings.Index(key, ":"); i > 0 {
				key = key[:i]
			}
			reasons[key]++
		}
		if i < *show {
			fmt.Printf("- %s [%s] %s\n  %s\n", t.Name, strings.Join(out.Categories, ","), out.Source, out.Summary)
		}
	}
	m := len(tools)
	if m == 0 {
		return errors.New("no tools with a README yet: run discovery and the health stage first")
	}
	fmt.Printf("\nmodel %s on %d tools\n", cfg.LLMModel, m)
	fmt.Printf("accepted: %d%% (first try %d%%), fallback: %d%%, avg latency %s\n",
		100*pass/m, 100*firstTry/m, 100*(m-pass)/m, (total / time.Duration(m)).Round(time.Millisecond))
	for r, c := range reasons {
		fmt.Printf("  rejected %3dx: %s\n", c, r)
	}
	if pass > 0 {
		avgIn, avgOut := float64(inTok)/float64(pass), float64(outTok)/float64(pass)
		fmt.Printf("avg tokens: %.0f in / %.0f out per tool\n", avgIn, avgOut)
		if *priceIn > 0 || *priceOut > 0 {
			per := (avgIn**priceIn + avgOut**priceOut) / 1e6
			fmt.Printf("estimated cost: $%.5f per tool, about $%.2f for %d tools (retries add ~%d%%)\n",
				per, per*float64(*catalog), *catalog, 100*(pass-firstTry)/max(pass, 1))
		}
	}
	return nil
}
