package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	// snapshotPassthrough is the size below which a snapshot goes back whole.
	// Forcing a file read for a trivial page costs an extra turn and saves
	// nothing; example.com renders to well under this.
	snapshotPassthrough = 2 << 10

	// snapshotInlineCap bounds what actually reaches the model. Measured, a
	// dense page (Hacker News) needs about 11 KB rendered, so a normal page is
	// never cut and a pathological one is bounded well inside the 64 KiB that
	// Bash output is allowed.
	snapshotInlineCap = 16 << 10

	// snapshotKeep is how many spilled snapshots survive on the overlay.
	snapshotKeep = 20
)

// snapFile matches the files this store owns, so rotation never touches
// anything else that ends up in the directory.
var snapFile = regexp.MustCompile(`^snap-(\d{4,})\.txt$`)

// snapshotStore spills full page snapshots to disk and hands the model a capped
// view plus a path.
//
// A snapshot is 6-11k tokens on a content-rich page and every tool result is
// re-sent on every later turn, so a handful of them come to dominate the cost
// of a conversation. The TypeScript agent measured one session re-reading
// 119,598 cached tokens in a single turn before it grew an equivalent of this.
type snapshotStore struct {
	dir string

	mu  sync.Mutex
	seq int
}

// newSnapshotStore prepares the directory and resumes the sequence.
//
// Resuming matters more than it looks. The TypeScript version kept the counter
// in memory, so after a restart it wrote snap-0001.txt again -- over a file
// whose path was already sitting in the resumed conversation. The model would
// follow that path and silently read a different page. Numbering continues from
// what is on disk, exactly as OpenLog continues event ids.
func newSnapshotStore(agentDir string) (*snapshotStore, error) {
	dir := filepath.Join(agentDir, "snapshots")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	s := &snapshotStore{dir: dir}
	s.seq = highestSeq(dir)
	return s, nil
}

// highestSeq finds the largest sequence number already written.
func highestSeq(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	high := 0
	for _, e := range entries {
		if n := seqOf(e.Name()); n > high {
			high = n
		}
	}
	return high
}

// seqOf extracts a snapshot file's sequence number, or 0 if it is not one.
func seqOf(name string) int {
	m := snapFile.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// digest returns what the model should see for one rendered snapshot, spilling
// the full text to disk when it is large enough to be worth retrieving.
//
// It fails open. A snapshot that cannot be written is still worth having in
// truncated form, so a full disk degrades the result rather than losing the
// page -- but unlike the TypeScript version, which logged to /tmp where nothing
// could see it, the failure is returned for the caller to put in the event log.
func (s *snapshotStore) digest(full string) (text string, err error) {
	if len(full) <= snapshotPassthrough {
		return full, nil
	}
	path, err := s.write(full)
	if err != nil {
		return capTextAt(full, snapshotInlineCap), err
	}
	return trim(full, path), nil
}

// write stores one snapshot and rotates the old ones out.
func (s *snapshotStore) write(full string) (string, error) {
	s.mu.Lock()
	s.seq++
	path := filepath.Join(s.dir, fmt.Sprintf("snap-%04d.txt", s.seq))
	s.mu.Unlock()
	if err := os.WriteFile(path, []byte(full), 0o640); err != nil {
		return "", err
	}
	s.rotate()
	return path, nil
}

// trim cuts a snapshot to the inline cap and says where the rest is.
//
// The trailer names an ABSOLUTE path and both tools that can retrieve it. The
// path has to be absolute: a relative one resolves against the workspace, not
// the agent's own directory, so the model would be sent somewhere that does not
// exist. Naming Read and Grep matters too -- the model reaches for whatever the
// text suggests, and Grep is the right tool for a 500 KB tree.
func trim(full, path string) string {
	if len(full) <= snapshotInlineCap {
		return full + "\n\n[full snapshot: " + path + "]"
	}
	cut := full[:snapshotInlineCap]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n\n[snapshot truncated here]\n[full snapshot: " + path +
		" - use Read or Grep on it to see the rest]"
}

// rotate keeps the newest snapshots and deletes the rest.
//
// By sequence number, not mtime: the number is monotonic and is what the paths
// in the conversation refer to, so ordering by it can never disagree with them.
func (s *snapshotStore) rotate() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	var seqs []int
	for _, e := range entries {
		if n := seqOf(e.Name()); n > 0 {
			seqs = append(seqs, n)
		}
	}
	if len(seqs) <= snapshotKeep {
		return
	}
	sort.Sort(sort.Reverse(sort.IntSlice(seqs)))
	for _, n := range seqs[snapshotKeep:] {
		os.Remove(filepath.Join(s.dir, fmt.Sprintf("snap-%04d.txt", n)))
	}
}
