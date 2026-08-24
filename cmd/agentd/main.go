// Command agentd runs the Go multi-agent daemon.
//
// Phase 2: one agent, one tool, a durable event log and a conversation that
// survives a restart. There is no HTTP surface yet -- this exists to prove the
// harness and to be read, not to be deployed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"cracked/internal/agentd"
)

// agentID is the one agent Phase 2 runs. The roster arrives in Phase 6.
const agentID = "boss"

// system is a placeholder prompt. The real composition -- BASE_IDENTITY, the
// profile's role, the agent's own instructions, then BASE_LIMITS last -- lands
// in Phase 5.
const system = `You are a helpful agent running on a Linux machine. You can read files.
Keep replies short and direct. Answer, then stop.`

func main() {
	once := flag.String("once", "", "run one turn with this prompt, then exit")
	model := flag.String("model", "claude-sonnet-5", "model id")
	workspace := flag.String("workspace", ".", "directory the agent may read")
	stateDir := flag.String("state-dir", "./agent-state", "where the log and conversation live")
	sysFile := flag.String("system", "", "read the system prompt from this file instead of the built-in one")
	flag.Parse()

	if *once == "" {
		fmt.Fprintln(os.Stderr, "usage: agentd -once \"<prompt>\" [-model M] [-workspace DIR] [-state-dir DIR]")
		os.Exit(2)
	}
	if err := run(*once, *model, *workspace, *stateDir, *sysFile); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run builds the agent, takes one turn, and prints what the turn logged.
func run(prompt, model, workspace, stateDir, sysFile string) error {
	agent, err := build(model, workspace, stateDir, sysFile)
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

// build assembles the agent from the flags.
func build(model, workspace, stateDir, sysFile string) (*agentd.Agent, error) {
	tools, err := agentd.FileTools(workspace)
	if err != nil {
		return nil, err
	}
	sys, err := systemPrompt(sysFile)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(stateDir, "agents", agentID)
	return agentd.New(agentID, dir, model, sys, tools)
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
