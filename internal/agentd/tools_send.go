package agentd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"cracked/internal/agentapi"
)

// maxAttachment is the largest file an agent may send. The same number as
// maxUpload on purpose: the app carries these both ways over one transport, and
// a file it would refuse to bring in is one it cannot carry out.
const maxAttachment = maxUpload

// Attachment kinds, as the client switches on them.
const (
	kindImage = "image"
	kindFile  = "file"
)

// sendFileInput is the send_file tool's argument. Never put a comma in a
// description: comma separates jsonschema tag options, so the text is silently
// truncated there and the model gets a worse schema with no error anywhere.
type sendFileInput struct {
	Path string `json:"path" jsonschema:"required,description=Path to the file - relative to the workspace or absolute inside it"`
	Note string `json:"note" jsonschema:"description=One short line saying what it is"`
}

type sendScreenshotInput struct {
	Note string `json:"note" jsonschema:"description=One short line saying what they are looking at"`
}

// sendTools are how an agent hands something to the person.
//
// send_screenshot is built only for a browser agent. A machine with no browser
// has nothing on its screen worth photographing, and the tool would offer an
// empty desktop as an answer. That is one switch and not two: the same flag
// decides the browser surface, so the two cannot disagree.
func sendTools(r roots, d toolDeps, out *outbox) ([]anthropic.BetaTool, error) {
	ctors := []func() (anthropic.BetaTool, error){
		func() (anthropic.BetaTool, error) { return sendFileTool(r, d, out) },
	}
	if d.browser {
		ctors = append(ctors, func() (anthropic.BetaTool, error) { return sendScreenshotTool(d, out) })
	}
	return buildTools(ctors...)
}

// sendFileTool gives the person a file the agent produced.
func sendFileTool(r roots, d toolDeps, out *outbox) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[sendFileInput](
		"send_file",
		"Give the person a file, so it arrives in the chat and they can open it. Use it for "+
			"anything you made that they should keep: a report, a spreadsheet, a slide deck. "+
			"Telling them where a file sits on this machine is not delivering it - they have no "+
			"way to reach this disk.",
		func(ctx context.Context, in sendFileInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return toolText(sendFile(r, d, out, in)), nil
		})
}

// sendFile copies one file into the outbox and puts it in the conversation.
func sendFile(r roots, d toolDeps, out *outbox, in sendFileInput) string {
	full, refusal := checkSendable(r, in.Path)
	if refusal != "" {
		return refusal
	}
	seq, dst, err := out.reserve(safeName(filepath.Base(full)))
	if err != nil {
		return err.Error()
	}
	// The size comes from the copy and not from the earlier stat: they disagree
	// when the file is still being written, and the one the person received is
	// the true one.
	size, err := copyFile(full, dst)
	if err != nil {
		os.Remove(dst)
		return "could not send " + in.Path + ": " + err.Error()
	}
	return announce(d, agentapi.Attachment{
		Seq: seq, Name: filepath.Base(dst), Size: size,
	}, in.Note)
}

// checkSendable vets the source, returning the refusal to send back when it will
// not do.
//
// Everything the MODEL can get wrong is checked before a number is taken -- a
// bad path, a folder, something far too big -- because a reservation spent on a
// send that then failed leaves a hole in the very sequence the app groups a run
// of pictures by. A disk that fills mid-copy can still spend one, and that is
// the honest limit of this: it cannot be known in advance.
func checkSendable(r roots, path string) (string, string) {
	full, err := resolve(r, path)
	if err != nil {
		return "", err.Error()
	}
	info, err := os.Stat(full)
	switch {
	case err != nil:
		return "", "could not read " + path + ": " + err.Error()
	case info.IsDir():
		return "", path + " is a folder. Send one file at a time."
	case info.Size() > maxAttachment:
		return "", fmt.Sprintf("%s is %d MB and the most you can send is %d MB.",
			path, info.Size()>>20, maxAttachment>>20)
	}
	return full, ""
}

// sendScreenshotTool shows the person what is on the agent's screen.
func sendScreenshotTool(d toolDeps, out *outbox) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[sendScreenshotInput](
		"send_screenshot",
		"Show the person what is on your screen, as a picture in the chat. Use it while you are "+
			"researching or shopping, when seeing the thing is what lets them decide: three "+
			"laptops you found, a page of prices, a chart. Send one per option so they can "+
			"compare. This is for THEM to look at - it does not come back to you.",
		func(ctx context.Context, in sendScreenshotInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return toolText(sendScreenshot(d, out, in.Note)), nil
		})
}

// sendScreenshot photographs the display and puts it in the conversation.
func sendScreenshot(d toolDeps, out *outbox, note string) string {
	if out.dir == "" {
		return errNoOutbox.Error()
	}
	a, err := captureToOutbox(out)
	if err != nil {
		return "could not photograph the screen: " + err.Error()
	}
	return announce(d, a, note)
}

// captureToOutbox photographs the display and numbers the result.
//
// Captured under a temporary name first, so the number is taken only once there
// is something to number. A capture that fails must not spend a sequence number.
func captureToOutbox(out *outbox) (agentapi.Attachment, error) {
	if err := os.MkdirAll(out.dir, 0o750); err != nil {
		return agentapi.Attachment{}, err
	}
	f, err := os.CreateTemp(out.dir, "capturing-*.png")
	if err != nil {
		return agentapi.Attachment{}, err
	}
	f.Close()
	tmp := f.Name()
	defer os.Remove(tmp)
	defer os.Remove(thumbPath(tmp))
	if err := scrotTo(tmp); err != nil {
		return agentapi.Attachment{}, err
	}
	return numberCapture(out, tmp)
}

// numberCapture moves a finished capture, and its thumbnail, into place.
//
// Both files are kept, unlike a handoff card: the thumbnail is what draws in a
// list of several options and the full capture is what they open to read the
// page. That is why this does not go through thumbOf, which deletes the full one.
func numberCapture(out *outbox, tmp string) (agentapi.Attachment, error) {
	seq, dst, err := out.reserve("screen.png")
	if err != nil {
		return agentapi.Attachment{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return agentapi.Attachment{}, err
	}
	a := agentapi.Attachment{Seq: seq, Name: filepath.Base(dst), Size: sizeOf(dst)}
	if os.Rename(thumbPath(tmp), thumbPath(dst)) == nil {
		a.Thumb = thumbName(a.Name)
	}
	return a, nil
}

// thumbPath is where the thumbnail beside a capture lives.
func thumbPath(path string) string {
	return filepath.Join(filepath.Dir(path), thumbName(filepath.Base(path)))
}

// sizeOf is a file's size, or 0 when it cannot be read.
func sizeOf(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

// announce logs the attachment and says what to tell the model.
//
// Its own event type rather than a field on the reply. The person's client draws
// this as a bubble of its own, and an old gateway that does not know the type
// drops it silently instead of rendering a message with a dangling reference.
func announce(d toolDeps, a agentapi.Attachment, note string) string {
	a.Kind = kindFile
	if _, isImage := attachmentMIME(a.Name); isImage {
		a.Kind = kindImage
	}
	logEvent(d, Event{Type: "attachment", Text: note, Attachment: &a})
	return "Sent " + a.Name + " to the person. It is in the chat now."
}

// copyFile copies the source into the outbox and reports what it actually wrote.
//
// A copy and not a hardlink. Sent has to mean what they got: an agent that edits
// the working file afterwards must not retroactively change the file already
// sitting in a message on someone's phone.
//
// One byte past the limit is read on purpose. A file a background command is
// still writing grows between the stat above and this copy, and a LimitReader
// set AT the cap would stop dead and report success -- handing the person a
// fragment announced at a size that says it is whole. saveUpload guards the
// inbound side the same way.
func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(in, maxAttachment+1))
	if err == nil && n > maxAttachment {
		err = fmt.Errorf("it grew past %d MB while being sent", maxAttachment>>20)
	}
	// Closed here rather than deferred, and its error kept: a buffered write
	// only reaches the overlay on close, so a full disk surfaces HERE. Dropping
	// it would announce a short file as a delivered one.
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}
