package agentd

import (
	"os"
	"sync"
	"testing"
)

// The number naming an attachment is also the number the app groups by, and it
// has to survive a restart.
//
// The TypeScript agent kept its snapshot counter in memory and wrote
// snap-0001.txt again after a restart, over a file whose path was already in the
// conversation. Here the same bug is worse: it would serve different bytes for a
// URL already sitting in a message on someone's phone.
func TestAttachmentNumbersKeepCountingAfterARestart(t *testing.T) {
	dir := t.TempDir()
	_, first, err := newOutbox(dir).reserve("report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("the first one"), 0o640); err != nil {
		t.Fatal(err)
	}
	seq, second, err := newOutbox(dir).reserve("report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 || second == first {
		t.Fatalf("a restarted outbox handed out %d at %s, want 2 at a fresh name", seq, second)
	}
	if body, err := os.ReadFile(first); err != nil || string(body) != "the first one" {
		t.Errorf("the first attachment was overwritten: %q %v", body, err)
	}
}

// A resumed sequence must count only the outbox's own files, and both halves of
// a screenshot belong to one number.
func TestOutboxSeqOfReadsItsOwnNames(t *testing.T) {
	for name, want := range map[string]int{
		"0007-screen.png":       7,
		"0007-screen-thumb.png": 7,
		"0012-2026-report.pdf":  12,
		"notes.txt":             0,
		"7-screen.png":          0,
	} {
		if got := outboxSeqOf(name); got != want {
			t.Errorf("outboxSeqOf(%q) = %d, want %d", name, got, want)
		}
	}
}

// Several tests build an agent with no state directory at all. filepath.Join("",
// "outbox") is the relative path "outbox", so without the guard those runs would
// quietly create a folder in the package source tree.
func TestOutboxWithNoAgentDirWritesNothing(t *testing.T) {
	o := newOutbox("")
	if o.dir != "" {
		t.Fatalf("outbox dir is %q, want empty", o.dir)
	}
	if _, _, err := o.reserve("report.pdf"); err == nil {
		t.Error("reserve succeeded with nowhere to write")
	}
	if _, err := os.Stat("outbox"); err == nil {
		os.RemoveAll("outbox")
		t.Error("an outbox was created in the working directory")
	}
}

// The SDK runs a turn's tool handlers concurrently, so two sends in one turn
// race for the next number. A number handed out twice is two pictures the app
// cannot tell apart.
func TestConcurrentReservesGetDistinctNumbers(t *testing.T) {
	o := newOutbox(t.TempDir())
	const n = 20
	seqs := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, _, err := o.reserve("screen.png")
			if err != nil {
				t.Error(err)
			}
			seqs[i] = seq
		}()
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, s := range seqs {
		if seen[s] {
			t.Fatalf("number %d was handed out twice", s)
		}
		seen[s] = true
	}
}

// Only images are served as themselves. The AGENT chooses these filenames, so a
// web client rendering an agent-named .html from its own origin is the hole this
// closes.
func TestOnlyImagesAreServedAsThemselves(t *testing.T) {
	for name, wantImage := range map[string]bool{
		"0001-screen.png": true, "0002-photo.JPEG": true,
		"0003-report.pdf": false, "0004-note.html": false, "0005-icon.svg": false,
	} {
		mimeType, isImage := attachmentMIME(name)
		if isImage != wantImage {
			t.Errorf("attachmentMIME(%q) image=%v, want %v", name, isImage, wantImage)
		}
		if !isImage && mimeType != "application/octet-stream" {
			t.Errorf("attachmentMIME(%q) = %q, want a download", name, mimeType)
		}
	}
}
