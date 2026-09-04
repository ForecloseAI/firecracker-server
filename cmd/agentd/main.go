// Command agentd runs the Go multi-agent daemon: a roster of agents sharing one
// workspace, each with its own durable event log, conversation and memory, an
// HTTP surface with SSE and interrupt, and a browser driven through
// chrome-devtools-mcp.
//
// It is the only agent a guest runs.
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
	"runtime"
	"syscall"
	"time"

	// The zoneinfo database, embedded. The whole guest runs on the person's own
	// timezone, and the guest image is not required to ship /usr/share/zoneinfo
	// -- without this a named zone would silently resolve to UTC and every
	// "daily at 09:00" would fire at the wrong hour.
	_ "time/tzdata"

	"cracked/internal/agentd"
)

func main() {
	once := flag.String("once", "", "run one turn with this prompt and exit, instead of serving")
	addr := flag.String("addr", "127.0.0.1:8081", "address to serve on")
	model := flag.String("model", "", "model id, overriding the profile's own")
	profile := flag.String("profile", "boss", "profile to use for -once runs")
	maxLive := flag.Int("max-live-agents", 8, "how many agents may hold a goroutine at once")
	workspace := flag.String("workspace", ".", "directory the agent may read")
	stateDir := flag.String("state-dir", "./agent-state", "where the log and conversation live")
	skillsDir := flag.String("skills-dir", agentd.BuiltinSkillsDir,
		"directory of built-in skills, shipped read-only in the guest image")
	flag.Parse()
	// Assigned rather than threaded through: every agent needs it and nothing
	// chooses a different one, which is the same reason ChromeURL is a var.
	agentd.BuiltinSkillsDir = *skillsDir

	err := run(*once, *profile, *model, *workspace, *stateDir, *addr, *maxLive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run builds the supervisor and either takes one turn or serves.
func run(once, profile, model, workspace, stateDir, addr string, maxLive int) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// First, and before any goroutine exists: this assigns time.Local, and the
	// guest spends the rest of its life on the person's clock rather than UTC.
	agentd.AdoptZone(stateDir)
	catalog, err := agentd.LoadCatalog(filepath.Join(stateDir, "agent-types"))
	if err != nil {
		return err
	}
	sup, err := agentd.NewSupervisor(ctx, stateDir, workspace, catalog, model, maxLive)
	if err != nil {
		return err
	}
	defer sup.Close()
	log.Printf("agentd: model calls go through %s", sup.ModelRoute())
	if once != "" {
		return runOnce(sup, profile, once)
	}
	return serve(ctx, sup, addr)
}

// serve runs the HTTP surface until interrupted.
func serve(ctx context.Context, sup *agentd.Supervisor, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: agentd.NewServer(sup).Routes(),
		// No WriteTimeout: it is an absolute deadline and would cut every SSE
		// stream at the same age, however active the client is. ReadHeaderTimeout
		// is safe alongside it and is what keeps a stalled client from holding a
		// connection open forever once this binds something other than loopback.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("agentd listening on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// runOnce takes a single turn against one agent and prints what it logged.
// Kept alongside serve because it is the fastest way to exercise the loop
// without a client. The profile flag names which agent it addresses: "boss"
// unless a specialist is being tried out.
func runOnce(sup *agentd.Supervisor, profile, prompt string) error {
	if profile != agentd.BossID {
		// An error means it is already on the roster from an earlier run.
		_, _ = sup.Create(profile, profile)
	}
	agent, err := sup.Get(profile)
	if err != nil {
		return err
	}
	from := agent.Log().LastID()
	turnErr := agent.Turn(context.Background(), prompt)
	// The events this turn appended: the same data an SSE client receives.
	events, err := agent.Log().Since(from)
	if err != nil {
		return err
	}
	for _, e := range events {
		show(e)
	}
	reportMemory()
	return turnErr
}

// show renders one event for the terminal.
func show(e agentd.Event) {
	switch e.Type {
	case "text":
		fmt.Println(e.Text)
	case "tool_use":
		fmt.Printf("  [tool] %s %s\n", e.Tool, e.Input)
	case "state":
		fmt.Printf("  [state] %s\n", e.SessionState)
	case "error":
		fmt.Printf("  [error] %s\n", e.Message)
	case "approval_required":
		fmt.Printf("  [approval %s] %s\n", e.ApprovalID, e.Preview)
	case "question":
		fmt.Printf("  [question %s] %s\n", e.ApprovalID, e.Question)
	case "decision":
		fmt.Printf("  [decision %s] %s\n", e.ApprovalID, e.Decision)
	case "usage":
		fmt.Printf("  [usage] model=%s in=%d out=%d cache_read=%d cache_write=%d\n",
			e.Model, e.Usage.InputTokens, e.Usage.OutputTokens,
			e.Usage.CacheReadInputTokens, e.Usage.CacheCreationInputTokens)
	case "turn_complete":
		fmt.Printf("  [turn] ok=%v %dms\n", !e.IsError, e.DurationMS)
	}
}

// reportMemory prints the Go-heap side of the memory picture. Process RSS is
// sampled from outside (ps or /usr/bin/time -l), because the two diverge and
// both are worth tracking.
func reportMemory() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("  [mem] heap_alloc=%.1fMB heap_sys=%.1fMB goroutines=%d\n",
		float64(m.HeapAlloc)/(1<<20), float64(m.HeapSys)/(1<<20), runtime.NumGoroutine())
}
