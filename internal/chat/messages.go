package chat

import (
	"strconv"

	"cracked/internal/agentapi"
)

// Message kinds, as the app renders them.
const (
	kindText  = "text"
	kindEvent = "event"
	kindAsk   = "ask"
)

// fromMe is what the client compares against to draw a message on the right.
const fromMe = "me"

// projectMessage turns one guest event into a line of conversation, or reports
// that it is not one.
//
// Shared by the live stream and by history on purpose: two mappers would let a
// line render one way as it arrived and another way after a reload, and nothing
// would report the difference.
func projectMessage(ev agentapi.Event) (Message, bool) {
	m := Message{ID: strconv.Itoa(ev.ID), Time: ev.TS, From: ev.Agent}
	switch ev.Type {
	case "user":
		m.Kind, m.From, m.Text, m.File = kindText, fromMe, ev.Text, ev.File
	case "text":
		m.Kind, m.Text = kindText, ev.Text
	case "delegation", "task_start", "agent_message":
		m.Kind, m.Text = kindEvent, teamLine(ev)
	case "approval_required", "question":
		return askMessage(m, ev), true
	default:
		return Message{}, false
	}
	return m, true
}

// askMessage renders a raised hand: what is being asked, the detail the person
// needs to answer it, and what kind of answer it wants.
func askMessage(m Message, ev agentapi.Event) Message {
	m.Kind, m.Detail = kindAsk, ev.Preview
	m.Text = ev.Question
	if m.Text == "" {
		m.Text = "Allow " + ev.Tool + "?"
	}
	m.UI = askUIOf(ev)
	if ev.Shot != "" {
		// Relative to the API root the client already holds, NOT including /v1:
		// the client joins this onto a base URL that ends in /v1, so naming the
		// prefix here asks it for /v1/v1/... and the picture silently never loads.
		m.Shot = "/threads/" + ev.Agent + "/shots/" + ev.Shot
	}
	return m
}

// askUIOf reports how an ask should be answered.
//
// A tool gate has no UI of its own -- it is always allow or deny -- so it is
// reported as "approval". A question already carries its kind and, for a choice,
// the options the person may pick from.
func askUIOf(ev agentapi.Event) *AskUI {
	if ev.Type == "approval_required" {
		return &AskUI{Kind: askApproval}
	}
	ui := &AskUI{Kind: ev.Kind}
	if ui.Kind == "" {
		ui.Kind = askText // ask_human's own default
	}
	if ev.UI != nil {
		ui.Options = ev.UI.Options
	}
	return ui
}

// teamLine narrates working as a team, which is most of what a boss does and
// none of which is otherwise visible to the person.
func teamLine(ev agentapi.Event) string {
	switch {
	case ev.Type == "delegation":
		return "Handed " + ev.TaskTitle + " to " + ev.To
	case ev.Type == "task_start":
		return "Started: " + ev.TaskTitle
	case ev.From != "":
		return "Heard back from " + ev.From
	}
	return "Messaged " + ev.To
}

// typingOf reports whether a state event means the agent started or stopped
// working, and whether it was a state event at all.
func typingOf(ev agentapi.Event) (bool, bool) {
	if ev.Type != "state" {
		return false, false
	}
	return ev.SessionState == "working", true
}

// applyDecisions fills the verdict on each ask that has since been answered.
//
// History carries the outcome on the card itself; the live stream sends a
// separate ask_resolved frame, because by then the client is already showing the
// card and only needs to know it is settled.
func applyDecisions(msgs []Message, events []agentapi.Event) []Message {
	// Keyed by message id, never by position: msgs is filtered, so an index into
	// the event list would land on the wrong card.
	settled := map[string]agentapi.Event{}
	asks := map[string]string{}
	for _, ev := range events {
		switch ev.Type {
		case "approval_required", "question":
			asks[ev.ApprovalID] = strconv.Itoa(ev.ID)
		case "decision":
			settled[ev.ApprovalID] = ev
		}
	}
	for i, m := range msgs {
		if ev, ok := decisionFor(m.ID, asks, settled); ok {
			msgs[i].Verdict, msgs[i].VerdictTime = verdictOf(ev.Decision), ev.TS
		}
	}
	return msgs
}

// decisionFor finds the answer to the ask a message represents, if it has one.
func decisionFor(msgID string, asks map[string]string,
	settled map[string]agentapi.Event) (agentapi.Event, bool) {
	for approvalID, askID := range asks {
		if askID != msgID {
			continue
		}
		ev, ok := settled[approvalID]
		return ev, ok
	}
	return agentapi.Event{}, false
}

// verdictOf maps the guest's allow/deny onto the words the app uses.
func verdictOf(decision string) string {
	if decision == "allow" {
		return "approved"
	}
	return "denied"
}
