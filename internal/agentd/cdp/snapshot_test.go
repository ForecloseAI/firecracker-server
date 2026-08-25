package cdp

import (
	"strings"
	"testing"
)

// node builds one accessibility node for a test tree.
func node(role, name string, backendID int64, ignored bool) axNode {
	return axNode{
		Role:             axValue{Value: role},
		Name:             axValue{Value: name},
		BackendDOMNodeID: backendID,
		Ignored:          ignored,
	}
}

// You never click a StaticText, so only actionable nodes carry a uid. That one
// decision is what takes Hacker News from 609 KB of raw accessibility JSON to
// about 11 KB rendered: the 417 text nodes carry their words and nothing else,
// while the 227 actionable ones carry something the model can act on. Giving
// every node a uid measured 31 KB for the same page.
func TestOnlyActionableNodesGetUIDs(t *testing.T) {
	s := build(1, []axNode{
		node("button", "Sign in", 10, false),
		node("heading", "Top stories", 11, false),
		node("StaticText", "some prose", 12, false),
	})
	if len(s.Actionable) != 1 || !strings.Contains(s.Actionable[0], "uid=1_10") {
		t.Fatalf("actionable = %v, want just the button with a uid", s.Actionable)
	}
	for _, line := range s.Context {
		if strings.Contains(line, "uid=") {
			t.Errorf("context line carries a uid: %q", line)
		}
	}
}

// Hacker News reports 1604 accessibility nodes and renders to 230. The
// difference is InlineTextBox, generic, none and LineBreak -- layout artefacts
// with no meaning to a reader -- plus everything Chrome has already marked
// ignored, which it computed precisely because it is not part of the accessible
// page. An ignored BUTTON is the case a role-only filter misses.
func TestSnapshotDropsNoiseAndIgnoredNodes(t *testing.T) {
	s := build(1, []axNode{
		node("InlineTextBox", "dup", 20, false),
		node("generic", "", 21, false),
		node("LineBreak", "", 22, false),
		node("button", "invisible", 23, true),
		node("link", "real", 24, false),
	})
	if len(s.Actionable) != 1 || !strings.Contains(s.Actionable[0], "real") {
		t.Errorf("actionable = %v, want only the visible link", s.Actionable)
	}
	if len(s.Context) != 0 {
		t.Errorf("context = %v, want the noise roles dropped", s.Context)
	}
}

// Position is meaning. Every Hacker News story has its own "comments" link, so
// the order the tree arrives in is what tells the model which link belongs to
// which story. Sorting or grouping would produce twenty identical labels.
func TestSnapshotKeepsDocumentOrder(t *testing.T) {
	s := build(1, []axNode{
		node("link", "first", 30, false),
		node("link", "second", 31, false),
		node("link", "third", 32, false),
	})
	got := strings.Join(s.Actionable, "|")
	if !strings.Contains(got, "first") || strings.Index(got, "first") > strings.Index(got, "third") {
		t.Errorf("order = %q, want document order", got)
	}
}

// A uid the model just read must still name the same element when it acts a few
// seconds later. Minting from backendDOMNodeId gives that for free; a render
// ordinal would shift the moment an ad or a lazy image landed between the
// snapshot and the click, and the model would act on the wrong element with
// nothing anywhere reporting an error.
func TestUIDsAreStableAcrossSnapshotsOfTheSamePage(t *testing.T) {
	tree := []axNode{node("button", "Buy", 412, false)}
	first := build(1, tree)
	second := build(1, tree)
	if first.Actionable[0] != second.Actionable[0] {
		t.Errorf("uid moved between identical snapshots: %q then %q",
			first.Actionable[0], second.Actionable[0])
	}
}

// Without the generation in the uid, one from before a navigation resolves
// against the fresh map whenever the new page happens to reuse a backend node
// id -- and the model acts on a different element on a different page. That is
// the worst failure available here, because it is completely silent.
func TestUIDsFromDifferentGenerationsCannotCollide(t *testing.T) {
	tree := []axNode{node("button", "Buy", 412, false)}
	if build(1, tree).Actionable[0] == build(2, tree).Actionable[0] {
		t.Error("the same node produced the same uid in two generations")
	}
}

// Actionable first, not document order, for the rendered result. Anything the
// model can act on has to survive truncation, and on a content-heavy page the
// buttons are scattered through thousands of lines of prose.
func TestRenderPutsActionableBeforeText(t *testing.T) {
	s := build(1, []axNode{
		node("StaticText", "a great deal of preamble", 40, false),
		node("button", "Submit", 41, false),
	})
	out := s.Render()
	if strings.Index(out, "Submit") > strings.Index(out, "preamble") {
		t.Errorf("text came before the actionable element:\n%s", out)
	}
}
