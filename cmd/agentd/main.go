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

// agentID is the one agent Phase 2 runs. The roster arrives in Phase 6.
const agentID = "boss"

// system is a placeholder prompt. The real composition -- BASE_IDENTITY, the
// profile's role, the agent's own instructions, then BASE_LIMITS last -- lands
// in Phase 5.
const system = `You are a helpful agent working on a computer. You can read, write and edit
files in your workspace, search them, and run shell commands.

Reading, writing, searching and running ordinary commands are yours to do
freely. Commands that could destroy data or the machine will pause and ask the
person first. If they say no, stop, and do not look for another way to do the
same thing.

Use ask_human when you genuinely need the person. Keep the question to one
short sentence.

Keep replies short and direct. Answer, then stop.`

func main() {
	once := flag.String("once", "", "run one turn with this prompt and exit, instead of serving")
	addr := flag.String("addr", "127.0.0.1:8081", "address to serve on")
	model := flag.String("model", "claude-sonnet-5", "model id")
	workspace := flag.String("workspace", ".", "directory the agent may read")
	stateDir := flag.String("state-dir", "./agent-state", "where the log and conversation live")
	sysFile := flag.String("system", "", "read the system prompt from this file instead of the built-in one")
	flag.Parse()

	agent, err := build(*model, *workspace, *stateDir, *sysFile)
	if err == nil && *once != "" {
		err = runOnce(agent, *once)
	} else if err == nil {
		err = serve(agent, *addr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// serve runs the agent's goroutine and its HTTP surface until interrupted.
func serve(agent *agentd.Agent, addr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go agent.Run(ctx)
	srv := &http.Server{Addr: addr, Handler: agentd.NewServer(agent).Routes()}
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

// runOnce takes a single turn and prints what it logged. Kept alongside serve
// because it is the fastest way to exercise the loop without a client.
func runOnce(agent *agentd.Agent, prompt string) error {
	from := agent.Log().LastID()
	turnErr := agent.Turn(context.Background(), prompt)
	if err := replay(agent, from); err != nil {
		return err
	}
	reportMemory()
	return turnErr
}

// build assembles the agent from the flags.
func build(model, workspace, stateDir, sysFile string) (*agentd.Agent, error) {
	sys, err := systemPrompt(sysFile)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(stateDir, "agents", agentID)
	return agentd.New(agentID, dir, workspace, model, sys)
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

// systemPrompt returns the built-in prompt, or the contents of a file. The
// override exists to exercise prompt caching, which silently does nothing
// below a ~1024-token prefix -- so the built-in placeholder is far too short
// to prove the cache breakpoint is placed correctly.
func systemPrompt(path string) (string, error) {
	if path == "" {
		return system, nil
	}
	buf, err := os.ReadFile(path)
	return string(buf), err
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
