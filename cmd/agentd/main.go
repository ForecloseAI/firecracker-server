// Command agentd runs the Go multi-agent daemon.
//
// Phase 3: one agent, one tool, a durable event log, a conversation that
// survives a restart, and an HTTP surface with SSE and interrupt. Not deployed
// anywhere yet -- the TypeScript agent still runs in every VM.
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

	"cracked/internal/agentd"
)

// agentID is the one agent this phase runs. The roster arrives next.
const agentID = "boss"

func main() {
	once := flag.String("once", "", "run one turn with this prompt and exit, instead of serving")
	addr := flag.String("addr", "127.0.0.1:8081", "address to serve on")
	model := flag.String("model", "", "model id, overriding the profile's own")
	profile := flag.String("profile", "boss", "profile to use for -once runs")
	maxLive := flag.Int("max-live-agents", 8, "how many agents may hold a goroutine at once")
	workspace := flag.String("workspace", ".", "directory the agent may read")
	stateDir := flag.String("state-dir", "./agent-state", "where the log and conversation live")
	flag.Parse()

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
	catalog, err := agentd.LoadCatalog(filepath.Join(stateDir, "agent-types"))
	if err != nil {
		return err
	}
	sup, err := agentd.NewSupervisor(ctx, stateDir, workspace, catalog, model, maxLive)
	if err != nil {
		return err
	}
	defer sup.Close()
	if once != "" {
		return runOnce(sup, profile, once)
	}
	return serve(ctx, sup, addr)
}

// serve runs the HTTP surface until interrupted.
func serve(ctx context.Context, sup *agentd.Supervisor, addr string) error {
	srv := &http.Server{Addr: addr, Handler: agentd.NewServer(sup).Routes()}
	// No WriteTimeout: it is an absolute deadline and would cut every SSE
	// stream at the same age, however active the client is.
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
	id := profile
	if id != agentd.BossID {
		if _, err := sup.Create(profile, profile); err != nil {
			// Already on the roster from an earlier run, which is fine.
			_ = err
		}
	}
	agent, err := sup.Get(id)
	if err != nil {
		return err
	}
	from := agent.Log().LastID()
	turnErr := agent.Turn(context.Background(), prompt)
	if err := replay(agent, from); err != nil {
		return err
	}
	reportMemory()
	return turnErr
}

// replay prints the events this turn appended, which is the same data a Phase 3
// SSE client will receive.
func replay(agent *agentd.Agent, from int) error {
	events, err := agent.Log().Since(from)
	if err != nil {
		return err
	}
	for _, e := range events {
		show(e)
	}
	return nil
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
// the plan tracks both from Phase 1 rather than measuring at the end.
func reportMemory() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("  [mem] heap_alloc=%.1fMB heap_sys=%.1fMB goroutines=%d\n",
		float64(m.HeapAlloc)/(1<<20), float64(m.HeapSys)/(1<<20), runtime.NumGoroutine())
}
