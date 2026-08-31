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

// thumbPercent is how far scrot shrinks the thumbnail it writes beside a
// capture: small enough to draw in a list, large enough to recognise a page.
const thumbPercent = "40"

// shotsDir is where an agent keeps the screens it captured.
func shotsDir(agentDir string) string { return filepath.Join(agentDir, "shots") }

// captureScreen grabs the guest's display and returns the file name, or "" when
// it could not.
//
// Never returns an error. A missing thumbnail is a worse card; a failed ask is a
// stuck agent.
func captureScreen(agentDir, name string) string {
	dir := shotsDir(agentDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ""
	}
	path := filepath.Join(dir, name)
	if err := scrotTo(path); err != nil {
		return ""
	}
	return thumbOf(path, name)
}

// scrotTo photographs the display into path, leaving a thumbnail beside it.
//
// The whole display, not the browser viewport: what the person is shown should
// be what they would actually see, which may be a dialog outside the page or a
// second window. The MCP screenshot tool can only photograph a tab.
func scrotTo(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), shotTimeout)
	defer cancel()
	// -o overwrites, -t writes a thumbnail beside the full capture.
	cmd := exec.CommandContext(ctx, "scrot", "--overwrite", "--thumb", thumbPercent, path)
	cmd.Env = append(os.Environ(), "DISPLAY=:0")
	return cmd.Run()
}

// thumbName is what scrot calls the thumbnail it leaves beside a capture.
func thumbName(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)] + "-thumb" + ext
}

// thumbOf prefers the thumbnail scrot wrote beside the full capture, falling
// back to the full one when only that exists.
//
// scrot --thumb writes "<name>-thumb.<ext>" as a SECOND file rather than
// shrinking the original, so taking the name we asked for would ship the full
// size and leave the thumbnail on disk unread.
//
// It also DELETES the full capture, which is right for a handoff card: it draws
// one small picture and there is nothing to tap through to. An attachment keeps
// both, which is why sendScreenshot does not reuse this.
func thumbOf(path, name string) string {
	thumb := thumbName(name)
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), thumb)); err == nil {
		os.Remove(path)
		return thumb
	}
	return name
}
