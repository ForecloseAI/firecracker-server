package agentd

import (
	"os"
	"testing"
	"time"
)

// TestMain puts the whole package on a zone nothing here asserts against.
//
// The design's load-bearing rule is that no code in this daemon may read
// time.Local for its answer: AdoptZone sets it once at boot, and a zone change
// afterwards moves only TZ, so a machine onboarded since its last restart still
// has UTC in-process. Everything that needs the person's clock takes it from
// personNow or an explicit location instead.
//
// That rule was written in three doc comments and enforced nowhere, which meant
// a regression was invisible on any developer machine whose own zone happened
// to match the fixture. Pinning to +23:17 -- an offset no real place keeps and
// no test here expects -- makes a stray time.Local read produce a visibly wrong
// date in whichever existing assertion covers that path.
//
// It is a tripwire, not a proof: it only catches paths some test already
// exercises with a time assertion. It also replaces two hand-rolled pins that
// each carried their own save-and-restore.
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("test-not-a-real-zone", 23*3600+17*60)
	os.Exit(m.Run())
}
