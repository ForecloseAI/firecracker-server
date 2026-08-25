package cdp

import (
	"context"
	"fmt"
	"strings"
)

// Roles the model can act on. These are the only nodes that get a uid, and that
// is the economy the whole snapshot budget rests on: measured on Hacker News,
// the actionable nodes are 227 of 1604 and render to 7 KB, while giving every
// node a uid pushed the same page past 31 KB.
var actionable = map[string]bool{
	"button": true, "link": true, "textbox": true, "combobox": true,
	"checkbox": true, "radio": true, "menuitem": true, "menuitemcheckbox": true,
	"menuitemradio": true, "tab": true, "slider": true, "searchbox": true,
	// spinbutton is <input type=number>. It was missing, so number fields had no
	// uid at all and could not be filled. There is no "textarea" role to add
	// beside it -- Chrome reports a <textarea> as textbox.
	"switch": true, "option": true, "spinbutton": true,
}

// Roles that tell the model where it is without being targets themselves.
var orientation = map[string]bool{
	"RootWebArea": true, "heading": true, "navigation": true, "main": true,
	"form": true, "dialog": true, "alert": true, "table": true, "list": true,
}

// Roles that are pure noise. InlineTextBox duplicates its StaticText parent
// line for line, and generic/none carry no meaning at all -- together they were
// most of the 502 KB a Wikipedia article returns.
var noise = map[string]bool{
	"InlineTextBox": true, "none": true, "generic": true, "LineBreak": true,
}

// axNode is the part of an accessibility node we use.
type axNode struct {
	Role             axValue      `json:"role"`
	Name             axValue      `json:"name"`
	BackendDOMNodeID int64        `json:"backendDOMNodeId"`
	Ignored          bool         `json:"ignored"`
	Properties       []axProperty `json:"properties"`
}

// axProperty is one accessibility state, such as checked or disabled.
type axProperty struct {
	Name  string  `json:"name"`
	Value axValue `json:"value"`
}

// axValue is CDP's boxed value shape.
type axValue struct {
	Value any `json:"value"`
}

// str renders a boxed value, which Chrome types loosely.
func (v axValue) str() string {
	s, _ := v.Value.(string)
	return strings.Join(strings.Fields(s), " ")
}

// truthy reads a boxed value Chrome types as a bool for some properties and as
// a string for others -- "checked" arrives as a tristate token, "disabled" as a
// plain boolean.
func (v axValue) truthy() bool {
	switch t := v.Value.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	}
	return false
}

// Target is one element a uid names: what to act on, and what it is.
//
// The role lets fill refuse a button instead of focusing it and typing into
// nothing. The name is the more valuable half: it lets every result say
// `clicked link "Sign in"` rather than `clicked uid=3_412`, which is the only
// thing that would let the model notice it acted on the wrong element.
type Target struct {
	NodeID int64
	Role   string
	Name   string
}

// Label renders a target the way the snapshot showed it.
func (t Target) Label() string { return describe(t.Role, t.Name) }

// Snapshot is one reading of a page and the uid map it produced.
//
// The uid is the contract: the model sees "uid=3_412" and hands it back to act
// on that element. Nothing in the browser surface takes a CSS selector, on
// purpose -- a model writing its own selector picks a fragile one and then
// silently acts on the wrong element, which is far worse than a call that fails.
type Snapshot struct {
	Gen        int
	nodes      map[string]Target // uid -> what it names
	Actionable []string          // rendered lines, uid-prefixed
	Context    []string          // orientation and text, no uids
}

// Take reads the page's accessibility tree and builds a fresh uid generation.
func (b *Browser) Take(ctx context.Context) (*Snapshot, error) {
	var out struct {
		Nodes []axNode `json:"nodes"`
	}
	err := b.conn.Call(ctx, b.sessionID, "Accessibility.getFullAXTree", nil, &out)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gen++
	b.snap = build(b.gen, out.Nodes)
	return b.snap, nil
}

// build turns raw accessibility nodes into one generation of uids and lines.
func build(gen int, nodes []axNode) *Snapshot {
	s := &Snapshot{Gen: gen, nodes: map[string]Target{}}
	for _, n := range nodes {
		role := n.Role.str()
		if n.Ignored || noise[role] {
			continue
		}
		switch {
		case actionable[role]:
			s.addActionable(role, n)
		case orientation[role]:
			s.Context = append(s.Context, describe(role, n.Name.str()))
		case role == "StaticText" && n.Name.str() != "":
			s.Context = append(s.Context, n.Name.str())
		}
	}
	return s
}

// addActionable records one targetable element and the uid the model will use.
func (s *Snapshot) addActionable(role string, n axNode) {
	uid := fmt.Sprintf("%d_%d", s.Gen, n.BackendDOMNodeID)
	name := n.Name.str()
	s.nodes[uid] = Target{NodeID: n.BackendDOMNodeID, Role: role, Name: name}
	s.Actionable = append(s.Actionable, "uid="+uid+" "+describe(role, name)+flagsOf(n))
}

// flagsOf renders the states that change what an action would do.
//
// Without [checked] the model cannot tell whether clicking a box ticks it or
// unticks it, so telling it to use click is a coin flip that silently unticks
// something the person needed. A [disabled] control accepts the CDP click and
// does nothing, which is the same silent failure on the other side.
func flagsOf(n axNode) string {
	var out string
	for _, p := range n.Properties {
		if p.Name == "checked" && p.Value.truthy() {
			out += " [checked]"
		}
		if p.Name == "disabled" && p.Value.truthy() {
			out += " [disabled]"
		}
	}
	return out
}

// describe renders a role and its accessible name.
func describe(role, name string) string {
	if name == "" {
		return role
	}
	return role + ` "` + name + `"`
}

// Resolve turns a uid from the model into a node id it can act on.
//
// Two failures are possible and they are different: a uid from an older
// generation means the page moved under the model, and a uid this snapshot
// never issued means it invented one. Both come back as text a tool can hand
// straight to the model, never as an error, so it can recover in one step
// instead of treating the turn as broken.
func (b *Browser) Resolve(uid string) (Target, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snap == nil {
		return Target{}, "the page has changed since that snapshot - take a new one first"
	}
	t, ok := b.snap.nodes[uid]
	if !ok {
		return Target{}, fmt.Sprintf("no element %q on the current snapshot - take a new one first", uid)
	}
	return t, ""
}

// SnapshotGen reports the live uid generation, or 0 when none survives. An
// action that navigated the page can tell the model so directly instead of
// letting it discover it as a refusal on the next call.
func (b *Browser) SnapshotGen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snap == nil {
		return 0
	}
	return b.snap.Gen
}

// Render lays the snapshot out actionable-first.
//
// Document order would be the obvious choice and is the wrong one: anything the
// model can act on has to survive truncation, and on a content-heavy page the
// buttons are scattered through thousands of lines of prose. Putting them first
// means a cut result is still a usable one.
func (s *Snapshot) Render() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("snapshot %d - %d elements you can act on\n\n", s.Gen, len(s.Actionable)))
	b.WriteString(strings.Join(s.Actionable, "\n"))
	if len(s.Context) > 0 {
		b.WriteString("\n\n--- page text ---\n")
		b.WriteString(strings.Join(s.Context, "\n"))
	}
	return b.String()
}
