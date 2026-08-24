package agentd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// bashTimeout bounds one command. Long enough for a build, short enough that a
// wedged command does not hold the turn open until the model's own limits bite.
const bashTimeout = 120 * time.Second

// bashOutputCap bounds what comes back. Command output enters the conversation
// and is re-sent on every later turn, so an unbounded `find /` would be paid
// for repeatedly.
const bashOutputCap = 64 << 10

// destructive are the commands that pause and ask. Carried over from the
// TypeScript gate, which is the list this system has actually been run with.
//
// This is a denylist and therefore leaky by construction: it catches the
// obvious ways to destroy the machine, not every possible one. The real
// containment is that the guest is a disposable microVM firewalled off from the
// host, the VPC and every other guest. This list exists to stop an honest
// mistake, not a determined one.
var destructive = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+-[a-z]*r`),
	regexp.MustCompile(`\bdd\s`),
	regexp.MustCompile(`\bmkfs`),
	regexp.MustCompile(`\bshutdown\b`),
	regexp.MustCompile(`\breboot\b`),
	regexp.MustCompile(`>\s*/dev/`),
	regexp.MustCompile(`git\s+push\s+.*--force`),
	regexp.MustCompile(`curl[^|]*\|\s*(sh|bash)`),
}

// bashInput is the Bash tool's argument.
type bashInput struct {
	Command string `json:"command" jsonschema:"required,description=Shell command to run in the workspace"`
}

// isDestructive reports whether a command needs a human decision first.
func isDestructive(cmd string) bool {
	for _, re := range destructive {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// bashTool builds the Bash tool, gated on destructive commands.
func bashTool(root string, gate *Gate) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[bashInput](
		"Bash", "Run a shell command in the workspace and return its output.",
		func(ctx context.Context, in bashInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if isDestructive(in.Command) {
				if err := gate.Check(ctx, "Bash", "Run shell command: "+in.Command, in); err != nil {
					return toolText(err.Error()), nil
				}
			}
			return runCommand(ctx, root, in.Command), nil
		})
}

// runCommand executes one command, returning its output as a tool result.
//
// A non-zero exit is NOT an error to the caller: the model needs to see the
// exit code and stderr so it can fix the command itself, rather than the turn
// dying on a failed grep.
func runCommand(ctx context.Context, root, command string) anthropic.BetaToolResultBlockParamContentUnion {
	ctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return toolText(describeRun(out.String(), err, ctx.Err()))
}

// describeRun renders output, plus the exit status when something went wrong.
func describeRun(out string, runErr, ctxErr error) string {
	body := capTextAt(out, bashOutputCap)
	if ctxErr != nil {
		return fmt.Sprintf("%s\n[command stopped: %v]", body, ctxErr)
	}
	if runErr != nil {
		return fmt.Sprintf("%s\n[exit: %v]", body, runErr)
	}
	if body == "" {
		return "[no output]"
	}
	return body
}
