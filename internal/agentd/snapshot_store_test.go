package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bigSnapshot builds a render past the spill threshold with a marker buried
// deep enough that only the file can contain it.
func bigSnapshot(marker string) string {
	var b strings.Builder
	for i := 0; i < 4000; i++ {
		b.WriteString("uid=1_1 link \"a line of a page that goes on and on\"\n")
	}
	b.WriteString(marker + "\n")
	return b.String()
}

// mustStore builds a snapshot store over a temp directory.
func mustStore(t *testing.T) *snapshotStore {
	t.Helper()
	s, err := newSnapshotStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// THE one that matters. The TypeScript digest was inert for an entire commit
// while looking like it worked: files appeared in the snapshots directory on
// every call and roughly 23k tokens a turn kept accruing anyway, because the
// replacement went back in a shape the hook silently ignored. Asserting that a
// file exists proves nothing at all -- this asserts what the model receives.
func TestDigestReplacesWhatTheModelActuallySees(t *testing.T) {
	s := mustStore(t)
	full := bigSnapshot("ZZMARKERZZ")
	got, err := s.digest(full)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "ZZMARKERZZ") {
		t.Error("the model still received the whole snapshot")
	}
	if len(got) > snapshotInlineCap+512 {
		t.Errorf("returned %d bytes, want it capped near %d", len(got), snapshotInlineCap)
	}
	path := pathFromTrailer(t, got)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the path handed to the model does not open: %v", err)
	}
	if !strings.Contains(string(body), "ZZMARKERZZ") {
		t.Error("the spilled file is missing the part that was cut")
	}
}

// pathFromTrailer pulls the snapshot path out of the trailer the model reads.
func pathFromTrailer(t *testing.T, digest string) string {
	t.Helper()
	const marker = "[full snapshot: "
	i := strings.Index(digest, marker)
	if i < 0 {
		t.Fatalf("no trailer naming the full snapshot in:\n%s", digest[max(0, len(digest)-300):])
	}
	rest := digest[i+len(marker):]
	if j := strings.IndexAny(rest, " ]"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// A relative path would resolve against the workspace rather than the agent's
// own directory, so the model would be sent somewhere that does not exist and
// would conclude the snapshot was lost.
func TestDigestTrailerNamesAnAbsolutePath(t *testing.T) {
	s := mustStore(t)
	got, _ := s.digest(bigSnapshot("x"))
	if path := pathFromTrailer(t, got); !filepath.IsAbs(path) {
		t.Errorf("trailer path %q is not absolute", path)
	}
}

// example.com renders to a few hundred bytes -- the TypeScript agent measured
// take_snapshot at 114 tokens there. Writing that to a file and spending a Read
// to fetch it back costs more than the snapshot did.
func TestSmallSnapshotsPassThroughWhole(t *testing.T) {
	s := mustStore(t)
	small := "uid=1_1 link \"tiny page\""
	got, err := s.digest(small)
	if err != nil || got != small {
		t.Errorf("digest(%q) = %q, %v; want it returned untouched", small, got, err)
	}
	entries, _ := os.ReadDir(s.dir)
	if len(entries) != 0 {
		t.Errorf("wrote %d files for a small snapshot, want none", len(entries))
	}
}

// The TypeScript counter was a process variable. A restart reset it to zero, so
// snap-0001.txt was overwritten while a RESUMED conversation still carried that
// path in its history -- the model followed it and read a different page, with
// nothing reporting an error. OpenLog already resumes event ids from disk at
// boot; this is the same fix applied to filenames.
func TestSnapshotNumbersKeepCountingAfterRestart(t *testing.T) {
	dir := t.TempDir()
	first, err := newSnapshotStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.digest(bigSnapshot("ORIGINAL"))
	oldPath := filepath.Join(first.dir, "snap-0001.txt")

	restarted, err := newSnapshotStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted.digest(bigSnapshot("AFTER-RESTART"))

	body, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("snap-0001.txt vanished across the restart: %v", err)
	}
	if !strings.Contains(string(body), "ORIGINAL") {
		t.Error("a restart overwrote a snapshot whose path is still in history")
	}
}

// These files sit on the small /dev/vdb overlay, shared with the Chrome
// profile. Twenty is enough that a resumed conversation's older paths usually
// still resolve, and few enough that a long browsing session cannot fill the
// disk. Rotation keys on the number, not mtime, because the counter is
// monotonic and is what the paths in the conversation refer to.
func TestRotationKeepsTheNewestByNumber(t *testing.T) {
	s := mustStore(t)
	for i := 0; i < snapshotKeep+5; i++ {
		if _, err := s.digest(bigSnapshot("n")); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(s.dir)
	if len(entries) != snapshotKeep {
		t.Fatalf("kept %d snapshots, want %d", len(entries), snapshotKeep)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "snap-0001.txt")); err == nil {
		t.Error("the oldest snapshot survived rotation")
	}
	if _, err := os.Stat(filepath.Join(s.dir, "snap-0025.txt")); err != nil {
		t.Error("the newest snapshot was rotated away")
	}
}

// Losing the page in order to save context is the wrong trade. A digest that
// cannot write must degrade to the old expensive behaviour, never to a broken
// tool or an empty result -- and it must not be silent either, or a read-only
// overlay would quietly double the cost of every browsing turn while everything
// looked fine.
func TestDigestFailsOpenWhenItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory mode bits")
	}
	s := mustStore(t)
	if err := os.Chmod(s.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.dir, 0o750) })

	got, err := s.digest(bigSnapshot("STILL-HERE"))
	if err == nil {
		t.Error("an unwritable directory was reported as success")
	}
	if !strings.Contains(got, "STILL-HERE") && len(got) < snapshotInlineCap {
		t.Errorf("the snapshot was lost rather than degraded: %d bytes", len(got))
	}
	if strings.TrimSpace(got) == "" {
		t.Error("an empty result reads to the model as a failed call")
	}
}
