package agentd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"cracked/internal/agentapi"
)

// errNoOutbox is what a send tool refuses with when the agent has no state
// directory to keep sent files in, which only ever happens in a unit test.
var errNoOutbox = errors.New("there is nowhere to keep files for the person on this machine")

// outboxDir is where an agent keeps what it has SENT the person, beside the
// screens it captured for a handoff and the page snapshots it spilled.
//
// Empty when there is no agent directory. filepath.Join("", "outbox") is
// "outbox" -- a relative path that resolves against the working directory and
// would quietly create a folder in the package source tree on every test run.
// The same trap under() documents about filepath.Abs("").
func outboxDir(agentDir string) string {
	if agentDir == "" {
		return ""
	}
	return filepath.Join(agentDir, "outbox")
}

// outboxSeqOf extracts an attachment's sequence number, or 0 if it has none.
// The format is declared in agentapi because the gateway needs it too.
func outboxSeqOf(name string) int { return agentapi.AttachmentSeq(name) }

// outbox hands out the numbers that name what an agent sends.
//
// The sequence RESUMES from disk, which is the whole reason this is a type and
// not a counter. The TypeScript agent kept its snapshot number in memory, so
// after a restart it wrote snap-0001.txt again, over a file whose path was
// already in the conversation. Here the same bug is worse: a reused name serves
// different bytes for a URL already sitting in a message on the person's phone.
//
// The mutex is load-bearing. The SDK runs a turn's tool handlers concurrently,
// so two sends in one turn race for the next number.
//
// Nothing is ever removed, and that is a decision rather than an omission. The
// spilled-snapshot store next door keeps only its newest twenty, but a snapshot
// is scratch and an attachment is a message the person already received:
// pruning one breaks a picture in a conversation they can still scroll back to.
// The overlay is theirs and DELETE ?purge=true is what empties it.
type outbox struct {
	dir string

	mu  sync.Mutex
	seq int
}

// newOutbox prepares an agent's outbox, continuing where it left off.
//
// Deliberately no error return. Tools() builds one for every agent, including
// the ones tests build with no state directory, and an outbox that cannot write
// has to become a tool that refuses -- not an agent that will not start.
func newOutbox(agentDir string) *outbox {
	o := &outbox{dir: outboxDir(agentDir)}
	if o.dir != "" {
		o.seq = highestSeq(o.dir, outboxSeqOf)
	}
	return o
}

// reserve takes the next number and says where a file by that number goes.
//
// Called only once the source has been checked, never before: a reservation
// spent on a send that then failed would leave a hole in the very numbering the
// client groups by.
func (o *outbox) reserve(name string) (int, string, error) {
	if o.dir == "" {
		return 0, "", errNoOutbox
	}
	if err := os.MkdirAll(o.dir, 0o750); err != nil {
		return 0, "", err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	return o.seq, filepath.Join(o.dir, fmt.Sprintf("%04d-%s", o.seq, name)), nil
}

// readableName is the file as the person should read it, and attachmentMIME is
// how it may be served. Both come from agentapi: the gateway recomputes the same
// answers rather than trusting what this daemon puts on the wire, so there must
// be exactly one definition of them.
func readableName(name string) string { return agentapi.ReadableName(name) }

func attachmentMIME(name string) (string, bool) { return agentapi.AttachmentMIME(name) }
