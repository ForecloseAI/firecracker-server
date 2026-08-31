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
	regexp.MustCompile(`git\s+push\s+.*--force`),
	regexp.MustCompile(`curl[^|]*\|\s*(sh|bash)`),
}

// browserRoutes are the ways a shell command can reach the machine's Chrome.
//
// The hole is real and needs no ingenuity to find: Chrome runs in every VM on
// 127.0.0.1:9222 as this same user, Node has a global WebSocket, and puppeteer
// is bundled in the image. Without this an agent drives the browser from Bash
// with no approval and no trace, and the person watching sees
// "Bash: node /tmp/x.js" rather than "navigate to bank.com".
//
// Redirected rather than gated, which is the opposite of the destructive list
// and deliberate. Unlike rm -rf there IS a right way to do this, so an approval
// prompt would ask the person to adjudicate something that has a correct
// answer. Gating could not be relied on anyway: Gate.consume grants per TOOL,
// so one batch "allow" on any destructive Bash would cover these too.
//
// Note what is NOT here: a bare chrome or chromium word match. It would fire on
// `grep -r chrome /var/log` and on a researcher writing about browsers. These
// match invocation, not mention.
var browserRoutes = []*regexp.Regexp{
	regexp.MustCompile(`\b9222\b`),
	regexp.MustCompile(`devtools/(browser|page)/`),
	regexp.MustCompile(`puppeteer`),
	regexp.MustCompile(`chrome-devtools-mcp`),
	regexp.MustCompile(`--(remote-debugging|user-data-dir)`),
	// --headless is NOT Chrome's alone, so it only counts beside a chrome
	// binary. LibreOffice uses the same flag for every document conversion,
	// which the pdf, docx, xlsx and pptx skills call for constantly -- matching
	// it bare sent every `soffice --headless --convert-to pdf` to the browser
	// redirect. Found on a live VM: the agent spent a minute and twenty tool
	// calls before getting round it by hiding the flag in a shell variable,
	// which worked and reads like a fight with its own tools.
	//
	// Still narrower than the bare `chrome` match the list deliberately omits:
	// this needs the binary AND the flag, so `grep -r chrome /var/log` is
	// untouched.
	regexp.MustCompile(`\bchrom[a-z-]*\b[^|;]*--headless`),
	regexp.MustCompile(`--headless[^|;]*\bchrom`),
	regexp.MustCompile(`\b(pkill|killall)\b[^|;]*chrom`),
	regexp.MustCompile(`\bxdotool\b`),
}

// browserRedirect is what the model is told instead of being asked.
//
// The second sentence matters: `puppeteer` also matches a coder legitimately
// installing it for the person's own project, and without a door left open that
// task dead-ends in advice about tools the agent does not have.
const browserRedirect = "Chrome here is driven with the browser tools " +
	"(navigate_page take_snapshot click fill press_key) rather than from the shell. " +
	"If you are writing browser automation code for the person's own project instead of " +
	"driving this machine's browser, ask them first."

// reachesBrowser reports a command that drives Chrome behind the tools' back.
func reachesBrowser(cmd string) bool {
	for _, re := range browserRoutes {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// bashInput is the Bash tool's argument.
type bashInput struct {
	Command string `json:"command" jsonschema:"required,description=Shell command to run in the workspace"`
}

// devWrite finds a redirect into /dev/. Writing to a real device node can
// destroy a disk, so it belongs on the list -- but the naive pattern this was
// inherited with, `>\s*/dev/`, also matches `2>/dev/null`, which is in a large
// fraction of all shell commands ever written.
//
// It was caught by a live run: an agent's `find ... 2>/dev/null` stopped dead
// waiting for a human. RE2 has no lookahead, so the exception is a lookup
// rather than a cleverer regexp.
var devWrite = regexp.MustCompile(`>\s*(/dev/[A-Za-z0-9_]*)`)

// harmlessDevices are the pseudo-devices ordinary commands redirect to.
var harmlessDevices = map[string]bool{
	"/dev/null": true, "/dev/stdout": true, "/dev/stderr": true, "/dev/tty": true,
}

// isDestructive reports whether a command needs a human decision first.
func isDestructive(cmd string) bool {
	for _, re := range destructive {
		if re.MatchString(cmd) {
			return true
		}
	}
	return writesToADevice(cmd)
}

// writesToADevice reports a redirect into anything under /dev/ that is not one
// of the harmless pseudo-devices.
func writesToADevice(cmd string) bool {
	for _, m := range devWrite.FindAllStringSubmatch(cmd, -1) {
		if !harmlessDevices[m[1]] {
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
			if reachesBrowser(in.Command) {
				return toolText(browserRedirect), nil
			}
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
	// bash, not sh. In the guest /bin/sh is dash, while on a developer's mac it
	// is bash in POSIX mode -- so every [[ ]], array and pipefail the model
	// learned to write in dev would start failing only in production.
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", command)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Without this, a command that backgrounds a process leaves a grandchild
	// holding the output pipe, and Run blocks past the timeout AND past ctx
	// cancellation -- wedging the agent goroutine, and with it the supervisor's
	// shutdown wait, until systemd resorts to SIGKILL.
	cmd.WaitDelay = 5 * time.Second
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
