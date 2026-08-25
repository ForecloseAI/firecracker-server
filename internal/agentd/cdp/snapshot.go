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
	"switch": true, "option": true,
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
	Role             axValue `json:"role"`
	Name             axValue `json:"name"`
	BackendDOMNodeID int64   `json:"backendDOMNodeId"`
	Ignored          bool    `json:"ignored"`
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

// Snapshot is one reading of a page and the uid map it produced.
//
// The uid is the contract: the model sees "uid=3_412" and hands it back to act
// on that element. Nothing in the browser surface takes a CSS selector, on
// purpose -- a model writing its own selector picks a fragile one and then
// silently acts on the wrong element, which is far worse than a call that fails.
type Snapshot struct {
	Gen        int
	nodes      map[string]int64 // uid -> backendDOMNodeId
	Actionable []string         // rendered lines, uid-prefixed
	Context    []string         // orientation and text, no uids
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
	s := &Snapshot{Gen: gen, nodes: map[string]int64{}}
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
	s.nodes[uid] = n.BackendDOMNodeID
	s.Actionable = append(s.Actionable, "uid="+uid+" "+describe(role, n.Name.str()))
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
func (b *Browser) Resolve(uid string) (int64, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snap == nil {
		return 0, "the page has changed since that snapshot - take a new one first"
	}
	id, ok := b.snap.nodes[uid]
	if !ok {
		return 0, fmt.Sprintf("no element %q on the current snapshot - take a new one first", uid)
	}
	return id, ""
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
