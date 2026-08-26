package agentd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// shotTimeout bounds a capture. A handoff is a person waiting to be handed the
// screen, so a screenshot that is slow to arrive is worth less than the ask
// arriving now: it is given a couple of seconds and then given up on.
const shotTimeout = 3 * time.Second

// shotsDir is where an agent keeps the screens it captured.
func shotsDir(agentDir string) string { return filepath.Join(agentDir, "shots") }

// captureScreen grabs the guest's display and returns the file name, or "" when
// it could not.
//
// The whole display, not the browser viewport: a handoff hands the person this
// machine's screen, so what they should be shown is what they will actually see
// -- which may be a dialog outside the page, or a second window. The MCP
// screenshot tool can only photograph a tab.
//
// Never returns an error. A missing thumbnail is a worse card; a failed ask is a
// stuck agent.
func captureScreen(agentDir, name string) string {
	dir := shotsDir(agentDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ""
	}
	path := filepath.Join(dir, name)
	ctx, cancel := context.WithTimeout(context.Background(), shotTimeout)
	defer cancel()
	// -o overwrites, -t makes a thumbnail: the app draws this small, and a full
	// 1920x1080 PNG per handoff is bytes nobody looks at.
	cmd := exec.CommandContext(ctx, "scrot", "--overwrite", "--thumb", "40", path)
	cmd.Env = append(os.Environ(), "DISPLAY=:0")
	if err := cmd.Run(); err != nil {
		return ""
	}
	return thumbOf(path, name)
}

// thumbOf prefers the thumbnail scrot wrote beside the full capture, falling
// back to the full one when only that exists.
//
// scrot --thumb writes "<name>-thumb.<ext>" as a SECOND file rather than
// shrinking the original, so taking the name we asked for would ship the full
// size and leave the thumbnail on disk unread.
func thumbOf(path, name string) string {
	ext := filepath.Ext(name)
	thumb := name[:len(name)-len(ext)] + "-thumb" + ext
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), thumb)); err == nil {
		os.Remove(path)
		return thumb
	}
	return name
}
