package chat

import (
	"net/url"
	"strconv"
	"strings"

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
	case "scheduled":
		m.Kind, m.Text = kindEvent, scheduleLine(ev)
	case "compaction":
		m.Kind, m.Text = kindEvent, ev.Message
	case "attachment":
		if ev.Attachment == nil {
			return Message{}, false
		}
		// A text bubble that happens to carry a file, not a fourth kind. The
		// client can already see the attachment is there, and a new kind would
		// make every one of them switch on something they can test directly.
		m.Kind, m.Text, m.Attachment = kindText, ev.Text, attachmentOf(ev)
	case "approval_required", "question":
		return askMessage(m, ev), true
	default:
		return Message{}, false
	}
	return m, true
}

// scheduleLine says which standing job just started a turn.
//
// An event and not a text message on purpose. A scheduled fire is the person's
// own agent acting on a timer, not the person speaking, and rendering it as a
// message from them would put words in the transcript they never typed. The
// name is what makes it recognisable -- "check the deploy queue" arriving at
// 3am means nothing without "morning sweep" attached to it.
func scheduleLine(ev agentapi.Event) string {
	if ev.TaskTitle == "" {
		return "A scheduled task ran"
	}
	return "Scheduled: " + ev.TaskTitle
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
	// A connect card's URL comes from the GUEST, unlike a handoff's, which the
	// host mints. Set here rather than beside the handoff URL in buildThread and
	// in the feed, because this mapper is the one thing those two share -- so the
	// allow-list check cannot be present on one path and missing from the other.
	if ev.Kind == askConnect && ev.UI != nil && connectHostAllowed(ev.UI.URL) {
		m.URL = ev.UI.URL
	}
	if ev.Shot != "" {
		// Relative to the API root the client already holds, NOT including /v1:
		// the client joins this onto a base URL that ends in /v1, so naming the
		// prefix here asks it for /v1/v1/... and the picture silently never loads.
		m.Shot = "/threads/" + ev.Agent + "/shots/" + ev.Shot
	}
	return m
}

// attachmentOf resolves the guest's bare file names into URLs the app can fetch.
//
// Relative to the API root the client already holds, NOT including /v1: it joins
// this onto a base URL that already ends in /v1, so naming the prefix here asks
// for /v1/v1/... and the file silently never loads. The same trap as the handoff
// shot above, which is why it is spelled out twice.
func attachmentOf(ev agentapi.Event) *Attachment {
	a := ev.Attachment
	base := "/threads/" + ev.Agent + "/files/"
	out := &Attachment{Seq: a.Seq, Name: a.Display, Kind: a.Kind,
		Size: a.Size, URL: base + a.Name}
	if a.Thumb != "" {
		out.ThumbURL = base + a.Thumb
	}
	return out
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

// connectHostsSuffix is the domain a connect link must live under. A var so a
// test can point it somewhere harmless.
var connectHostsSuffix = "composio.dev"

// connectHostAllowed reports whether a connect link may be rendered as a button.
//
// This is the first URL on a card that the GUEST authors -- every other one, the
// handoff's included, is minted by the host. A card carries more trust than a
// link in a sentence, so a prompt-injected agent raising "Connect your Gmail"
// pointing at a page that merely looks like Google is a real attack. Failing
// this check drops the button and leaves the rest of the card, which degrades to
// the person reading the request and ignoring it.
func connectHostAllowed(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == connectHostsSuffix || strings.HasSuffix(host, "."+connectHostsSuffix)
}
