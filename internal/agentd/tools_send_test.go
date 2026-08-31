package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// sendSurface builds a tool set over a real workspace and agent directory.
func sendSurface(t *testing.T, browser bool) (string, string, []anthropic.BetaTool, *Log) {
	t.Helper()
	ws, agentDir, log := t.TempDir(), t.TempDir(), mustLog(t)
	tools, err := Tools(roots{workspace: ws, own: agentDir}, toolDeps{
		gate: NewGate(log, NewInteractions(), t.TempDir()), log: log, browser: browser,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws, agentDir, tools, log
}

// attachmentEvent returns the one attachment an agent logged.
func attachmentEvent(t *testing.T, log *Log) Event {
	t.Helper()
	events, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "attachment" {
			return e
		}
	}
	t.Fatal("nothing was logged as an attachment")
	return Event{}
}

// The path goes through the same confinement the file tools use, so an agent
// cannot hand the person something off this machine's root.
func TestSendFileRefusesOutsideTheRoots(t *testing.T) {
	_, agentDir, tools, _ := sendSurface(t, false)
	if out := call(t, tools, "send_file", map[string]any{"path": "/etc/hosts"}); !strings.Contains(out, "outside the workspace") {
		t.Fatalf("send_file said %q", out)
	}
	if entries, _ := os.ReadDir(outboxDir(agentDir)); len(entries) != 0 {
		t.Error("a refused send still wrote to the outbox")
	}
}

// What the person actually receives, and what the app needs to draw it.
func TestSendFileLogsAnAttachmentTheAppCanRender(t *testing.T) {
	ws, agentDir, tools, log := sendSurface(t, false)
	body := []byte("the third quarter")
	if err := os.WriteFile(filepath.Join(ws, "report.pdf"), body, 0o640); err != nil {
		t.Fatal(err)
	}
	out := call(t, tools, "send_file", map[string]any{"path": "report.pdf", "note": "the Q3 numbers"})
	// The model is told the name the person will see, not the one on disk.
	if !strings.Contains(out, "Sent report.pdf") {
		t.Fatalf("send_file said %q", out)
	}
	if _, err := os.Stat(filepath.Join(outboxDir(agentDir), "0001-report.pdf")); err != nil {
		t.Fatalf("the file never reached the outbox: %v", err)
	}
	e := attachmentEvent(t, log)
	if e.Text != "the Q3 numbers" {
		t.Errorf("the note is %q", e.Text)
	}
	a := e.Attachment
	if a == nil || a.Seq != 1 || a.Kind != kindFile || a.Size != int64(len(body)) || a.Thumb != "" {
		t.Errorf("attachment = %+v", a)
	}
	// Two names: the one that finds the file, and the one the person reads.
	if a.Name != "0001-report.pdf" || a.Display != "report.pdf" {
		t.Errorf("name %q display %q", a.Name, a.Display)
	}
}

// The sequence is what the app groups a run of pictures by, so a send that
// failed must not leave a hole in it.
func TestSendFileDoesNotSpendANumberOnFailure(t *testing.T) {
	ws, agentDir, tools, _ := sendSurface(t, false)
	if out := call(t, tools, "send_file", map[string]any{"path": "missing.pdf"}); !strings.Contains(out, "could not read") {
		t.Fatalf("a missing file was not refused: %q", out)
	}
	if err := os.WriteFile(filepath.Join(ws, "report.pdf"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	call(t, tools, "send_file", map[string]any{"path": "report.pdf"})
	if _, err := os.Stat(filepath.Join(outboxDir(agentDir), "0001-report.pdf")); err != nil {
		t.Errorf("the first send to succeed was not number one: %v", err)
	}
}

// A folder is a mistake the model can recover from, so it is told what went
// wrong rather than handed a broken tool.
func TestSendFileRefusesAFolder(t *testing.T) {
	ws, _, tools, _ := sendSurface(t, false)
	if err := os.Mkdir(filepath.Join(ws, "notes"), 0o750); err != nil {
		t.Fatal(err)
	}
	if out := call(t, tools, "send_file", map[string]any{"path": "notes"}); !strings.Contains(out, "one file at a time") {
		t.Errorf("send_file said %q", out)
	}
}

// A FIFO stats like a small file and then blocks os.Open until a writer appears.
// Nothing in the tool watches the turn's context, so without this check an agent
// that ran mkfifo through Bash could wedge itself with no way to interrupt it.
func TestSendFileRefusesSomethingThatIsNotAnOrdinaryFile(t *testing.T) {
	ws, agentDir, tools, _ := sendSurface(t, false)
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe"), 0o600); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}
	done := make(chan string, 1)
	go func() { done <- call(t, tools, "send_file", map[string]any{"path": "pipe"}) }()
	select {
	case out := <-done:
		if !strings.Contains(out, "not an ordinary file") {
			t.Errorf("send_file said %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send_file blocked on a fifo, which would wedge the agent")
	}
	if entries, _ := os.ReadDir(outboxDir(agentDir)); len(entries) != 0 {
		t.Error("a refused send still wrote to the outbox")
	}
}

// A machine with no browser has nothing on its screen worth photographing, so
// the tool is not built for one.
func TestSendScreenshotIsBrowserOnly(t *testing.T) {
	if _, _, tools, _ := sendSurface(t, false); hasTool(tools, "send_screenshot") {
		t.Error("an agent with no browser was given send_screenshot")
	}
	if _, _, tools, _ := sendSurface(t, true); !hasTool(tools, "send_screenshot") {
		t.Error("a browser agent did not get send_screenshot")
	}
}

// Membership of alwaysAllowed and the browser gate contradict each other:
// TestProfileToolListNarrowsTheSurface asserts every always-allowed name reaches
// an agent built with browser off, which this one deliberately does not.
func TestSendScreenshotIsNotAlwaysAllowed(t *testing.T) {
	if contains(alwaysAllowed, "send_screenshot") {
		t.Error("send_screenshot is in alwaysAllowed but is built only for a browser agent")
	}
	if !contains(alwaysAllowed, "send_file") {
		t.Error("send_file is not always allowed, so an agent must remember to ask for it")
	}
}
