package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cracked/internal/agentapi"
)

// sentPicture is one screenshot an agent sent the person.
func sentPicture(id int) agentapi.Event {
	e := ev(id, "attachment")
	e.Attachment = &agentapi.Attachment{
		Seq: 3, Name: "0003-screen.png", Kind: "image", Size: 4096,
		Thumb: "0003-screen-thumb.png",
	}
	return e
}

// One mapper for both paths, so an attachment cannot render one way as it
// arrives and another way after a reload.
//
// Compared as JSON rather than with ==: both paths build their own *Attachment,
// so a struct comparison tests pointer identity and fails on correct code.
func TestAttachmentStreamAndHistoryAgree(t *testing.T) {
	e := sentPicture(7)
	var out strings.Builder
	newFeed("m1", noHandoff).emitMessage(&out, "coder", e)
	var frame feedFrame
	if err := json.Unmarshal(payload(t, out.String()), &frame); err != nil {
		t.Fatal(err)
	}
	th := buildThread("coder", []agentapi.Event{e}, "")
	if len(th.Messages) != 1 {
		t.Fatal("history dropped the attachment")
	}
	live, _ := json.Marshal(frame.Message)
	history, _ := json.Marshal(&th.Messages[0])
	if string(live) != string(history) {
		t.Errorf("stream %s != history %s", live, history)
	}
}

// The client joins these onto a base URL that already ends in /v1, so naming the
// prefix here would ask for /v1/v1/... and the picture silently never loads.
func TestAttachmentURLsAreRelativeToTheApiRoot(t *testing.T) {
	m, ok := projectMessage(sentPicture(9))
	if !ok || m.Attachment == nil {
		t.Fatal("an attachment event did not become a message")
	}
	if strings.HasPrefix(m.Attachment.URL, "/v1/") {
		t.Errorf("url %q repeats the /v1 the client already has", m.Attachment.URL)
	}
	if m.Attachment.URL != "/threads/coder/files/0003-screen.png" {
		t.Errorf("url is %q", m.Attachment.URL)
	}
	if m.Attachment.ThumbURL != "/threads/coder/files/0003-screen-thumb.png" {
		t.Errorf("thumb url is %q", m.Attachment.ThumbURL)
	}
	// The app groups a run of pictures by this, so it has to survive the hop.
	if m.Attachment.Seq != 3 {
		t.Errorf("seq is %d, want 3", m.Attachment.Seq)
	}
	// The number is for grouping and for the URL, not for reading.
	if m.Attachment.Name != "screen.png" {
		t.Errorf("the name the person reads is %q", m.Attachment.Name)
	}
}

// The file the person is shown is named the way they would name it, while the
// URL keeps the number that actually finds it on disk.
func TestSentFilePreviewsWithoutItsSequencePrefix(t *testing.T) {
	e := ev(11, "attachment")
	e.Attachment = &agentapi.Attachment{Seq: 1, Name: "0001-report.pdf", Kind: "file", Size: 12}
	th := buildThread("coder", []agentapi.Event{e}, "")
	if th.LastMessage != "Sent report.pdf" {
		t.Errorf("thread preview is %q", th.LastMessage)
	}
	if got := th.Messages[0].Attachment.URL; got != "/threads/coder/files/0001-report.pdf" {
		t.Errorf("url is %q and must still find the file on disk", got)
	}
}

// A picture sent with no note carries no text, and a blank line on the thread
// list reads as nothing having happened.
func TestAttachmentWithNoNoteStillPreviews(t *testing.T) {
	th := buildThread("coder", []agentapi.Event{sentPicture(3)}, "")
	if th.LastMessage == "" {
		t.Fatal("the thread list preview is blank")
	}
	if th.Messages[0].Text != "" {
		t.Errorf("a stand-in was written into the transcript itself: %q", th.Messages[0].Text)
	}
}

// A bubble with a dangling reference is worse than no bubble.
func TestAttachmentWithNothingAttachedIsDropped(t *testing.T) {
	if _, ok := projectMessage(ev(4, "attachment")); ok {
		t.Error("an attachment event with nothing attached became a message")
	}
}

// The guest decided what may render inline and what must be downloaded. This is
// the hop that actually faces a browser, so dropping its headers here would make
// the care taken at the other end worth nothing.
func TestAttachmentProxyForwardsWhatTheGuestDecided(t *testing.T) {
	s, _, u := newFake(t)
	w := call(t, s, u, "GET", "/v1/threads/boss/files/0001-report.pdf", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type is %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Errorf("Content-Disposition is %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options is %q", got)
	}
	if w.Body.String() != "the report" {
		t.Errorf("body is %q", w.Body.String())
	}
}
