// Package agentapi is the wire format the guest daemon and the host share.
//
// It exists because these types were previously declared twice -- once in
// internal/agentd and once in internal/agent -- and hand-kept in sync. They
// drifted, silently and for a long time: the daemon stamped `model` and `agent`
// on every event and the host's copy had neither field, so both were discarded
// on decode and no cost could be attributed to anything. Nothing errored,
// because a JSON field with nowhere to go is simply dropped.
//
// One declaration makes that class of bug a compile error instead of a review
// problem. Both sides import this; neither redeclares it.
package agentapi

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BossID is the one agent id a client may assume exists. Every machine gets a
// boss at first boot and it cannot be deleted, so it is the address for anything
// that must reach "whoever is in charge here" without listing the roster first.
const BossID = "boss"

// Usage mirrors the SDK's usage block. There is deliberately no cost field: the
// Messages API returns token counts only, so events carry tokens plus the model
// id and pricing is applied host-side, where the table can change without
// rebuilding a guest image.
type Usage struct {
	// ClearedInputTokens and ClearedToolUses report what context editing
	// actually removed. They are the only evidence it is doing anything: a
	// clear_tool_uses edit that is configured but never fires looks exactly like
	// one that works, right up until the bill arrives.
	ClearedInputTokens int64 `json:"cleared_input_tokens,omitempty"`
	ClearedToolUses    int64 `json:"cleared_tool_uses,omitempty"`

	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// TotalTokens is every token billed across all four categories.
func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// UI tells a client how to render a pending interaction.
type UI struct {
	Kind    string   `json:"kind"` // text | confirm | choice | handoff | connect
	Options []string `json:"options,omitempty"`
	// URL is where a "connect" card sends the person to sign in.
	//
	// Unlike a handoff's URL, which the host mints, this one is authored in the
	// guest -- so the host checks its origin before letting a client render it
	// as a button. See connectHostAllowed in internal/chat.
	URL string `json:"url,omitempty"`
}

// Apps is the machine's connection to the person's app integrations: one
// tool-router session, minted by the host and pushed here because a guest
// cannot reach the host to ask for it.
//
// Config, not state. The host remembers having minted a session; this is the
// copy the daemon needs on its own disk to dial one. An empty SessionURL means
// this machine has no connected-apps surface, which is the ordinary shape when
// no integration provider is configured.
type Apps struct {
	SessionURL string `json:"session_url"`
	SessionID  string `json:"session_id,omitempty"`
	// ReadOnly is the actions the provider annotates as only reading, pushed
	// rather than compiled in: a tool the provider ships reaches a machine on its
	// next push, instead of waiting for a rootfs rebuild and a recreated VM. What
	// is absent from it asks a person, so an empty set is noisy, never permissive.
	//
	// A machine is pushed again once the set it was handed goes stale -- an hour
	// for a complete one, minutes for a partial -- on the next request to reach
	// that machine rather than on a timer, so an idle machine is not re-ticketed
	// for a set nobody is reading. See claimApps and doneApps.
	//
	// Pushed only. AppsStore persists the session and not this: it is the same
	// answer for everybody, it is the provider's to change, and a row that
	// carried it would go stale in a place nobody looks. That is also why this
	// struct is no longer comparable -- compare the fields you mean.
	ReadOnly []string `json:"read_only,omitempty"`

	// Policy is what the person allows their agents to do without being asked,
	// per app and per capability: {"gmail":{"write":"ask","del":"never"}}.
	//
	// The mirror of ReadOnly in every respect. That set is the provider's, the
	// same for everybody, and pushed rather than stored; this is one person's
	// answer, theirs alone, and stored rather than derived -- so unlike ReadOnly
	// it DOES belong in AppsStore, and a row that lost it would silently return
	// somebody to defaults they had deliberately changed.
	//
	// Absent means ask. An unknown capability or an unrecognised value means ask
	// too: this is the first thing that can make a machine less capable than
	// intended, so every way of being wrong about it points at asking.
	Policy map[string]map[string]string `json:"policy,omitempty"`

	// Actions is the answer the guest acts on: one entry per connected-app
	// action, resolved on the host from the provider's capability map and this
	// person's Policy above.
	//
	// Resolved rather than shipped in halves so the guest holds no vocabulary of
	// its own -- it looks up a string and obeys it, which is the same shape as
	// the read-only set it will replace, and the reason the name classifier could
	// be deleted from that package. Both ship today: nothing obeys Actions until
	// the whole fleet runs an image that does. Absent means ask.
	Actions map[string]string `json:"actions,omitempty"`
}

// What an agent may do with one connected-app action. The vocabulary is shared
// because the host resolves it and the guest obeys it, and a disagreement about
// the spelling would be a machine quietly asking about nothing, or about all of
// it.
const (
	ActionAsk   = "ask"
	ActionAuto  = "auto"
	ActionNever = "never"
)

// Decision is a human's answer to a pending interaction. Scope, MaxUses and
// TTLSeconds turn a single approval into a batch consent.
type Decision struct {
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Scope      string `json:"scope,omitempty"`
	MaxUses    int    `json:"max_uses,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// Raised is one agent waiting on a person: a raised hand.
//
// Agent is the point of it. Any agent may need the person -- every worker
// profile carries Bash and ask_human -- and the person must be able to see who
// is asking and answer that agent, rather than the boss answering on its behalf.
type Raised struct {
	ID       string          `json:"id"`
	Agent    string          `json:"agent"`
	Kind     string          `json:"kind"` // approval_required | question
	Tool     string          `json:"tool,omitempty"`
	Preview  string          `json:"preview,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Question string          `json:"question,omitempty"`
	UI       *UI             `json:"ui,omitempty"`
	// Shot names a screenshot of the machine's display, for a handoff.
	Shot  string    `json:"shot,omitempty"`
	Since time.Time `json:"since"`
}

// PendingChange is one hub event: a hand raised, or one lowered.
type PendingChange struct {
	Raised    *Raised `json:"raised,omitempty"`
	ClearedID string  `json:"cleared_id,omitempty"`
}

// Event is one entry in an agent's log. The field set is the union of what
// every event type needs, only the relevant ones being set.
type Event struct {
	ID    int       `json:"id"`
	Agent string    `json:"agent"`
	Type  string    `json:"type"`
	TS    time.Time `json:"ts"`

	Text  string          `json:"text,omitempty"`
	From  string          `json:"from,omitempty"`
	To    string          `json:"to,omitempty"`
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// File is what the person attached to this message, if anything.
	File *File `json:"file,omitempty"`
	// Attachment is what the AGENT sent back: a document it produced, or a
	// picture of its screen. The mirror of File, and deliberately a second
	// field rather than a direction flag on one -- the two are read by
	// different code on the client and carry different fields.
	Attachment *Attachment `json:"attachment,omitempty"`

	Model        string `json:"model,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	SessionState string `json:"session_state,omitempty"`
	Message      string `json:"message,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`

	// A pending interaction waiting on a human, and its resolution.
	ApprovalID string `json:"approval_id,omitempty"`
	Preview    string `json:"preview,omitempty"`
	Question   string `json:"question,omitempty"`
	Kind       string `json:"kind,omitempty"`
	UI         *UI    `json:"ui,omitempty"`
	Shot       string `json:"shot,omitempty"`
	Decision   string `json:"decision,omitempty"`

	// A task folder opened by start_task, or carried by a delegation.
	TaskSlug  string `json:"task_slug,omitempty"`
	TaskTitle string `json:"task_title,omitempty"`
	TaskDir   string `json:"task_dir,omitempty"`
}

// File is something the person sent from the app, once it is on the guest's
// disk. Path is absolute because agents are given it to read.
type File struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Attachment is something an agent sent the person: a document it produced, or a
// picture of its screen.
//
// Seq is dense, per-agent, and survives a restart, and it is also what names the
// file on disk. Dense is the point: it lets a client group a run of pictures the
// way a chat app does, because two attachments with consecutive Seq were sent one
// after the other. The event id cannot answer that -- ids advance on every event,
// so two pictures a turn apart and two pictures a second apart look alike.
type Attachment struct {
	Seq int `json:"seq"`
	// Name is the file on disk, sequence prefix and all, and it is what the
	// download URL is built from. Display is the same file as the person should
	// read it. Both, rather than one the host un-prefixes, because the guest
	// mints the prefix and is the only side that knows the format -- the same
	// reason File carries Name and Path separately.
	Name    string `json:"name"`
	Display string `json:"display"`
	// Kind is the guest's serving policy surfaced on the wire, not a second
	// opinion about it. The download route is handed a name and no event, so it
	// has to decide image-or-download from the extension anyway; this is that
	// same decision, computed by the same helper, so the two cannot disagree.
	Kind string `json:"kind"` // image | file
	Size int64  `json:"size"`
	// Thumb names a smaller copy sitting beside the full one, for a list. Only
	// screenshots have one: a chart an agent drew is sent whole.
	Thumb string `json:"thumb,omitempty"`
}

// The kinds an Attachment may be. Declared here beside the field they describe,
// so the daemon that stamps one and the gateway that renders it cannot drift
// into two private vocabularies.
const (
	KindImage = "image"
	KindFile  = "file"
)

// attachmentPrefix matches the sequence an outbox file is named with, and
// captures the number. Anchored, so "0007-screen-thumb.png" and
// "0007-2026-report.pdf" both belong to seven.
var attachmentPrefix = regexp.MustCompile(`^(\d{4,})-`)

// AttachmentSeq extracts an attachment's sequence number, or 0 if it has none.
func AttachmentSeq(name string) int {
	m := attachmentPrefix.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ReadableName is an attachment as the person should read it, without the
// sequence prefix the daemon named it with.
func ReadableName(name string) string {
	return attachmentPrefix.ReplaceAllString(name, "")
}

// attachmentImages are the only types an attachment is served as itself.
var attachmentImages = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// AttachmentMIME reports how to serve a file, and whether it is an image.
//
// An allowlist and not a MIME table, because the AGENT chooses these filenames
// and a page served as itself runs as itself.
//
// It lives HERE, in the package both sides share, because the gateway must
// compute this for itself rather than pass on what the guest said. A guest is
// the person's own machine and they have root on it: a daemon they patched
// could answer Content-Type: text/html, and the gateway serves that on the same
// origin as the operator console and its __Host-sess cookie. nosniff does not
// help against an explicitly declared type. So both hops ask this function, and
// the untrusted answer is never forwarded.
func AttachmentMIME(name string) (string, bool) {
	if t, ok := attachmentImages[strings.ToLower(filepath.Ext(name))]; ok {
		return t, true
	}
	return "application/octet-stream", false
}

// Person is what the machine knows about whoever it works for. Collected at
// onboarding and added to over time; it is rendered into a Markdown file the
// agents read, so Notes is prose rather than a structure.
type Person struct {
	Name      string `json:"name"`
	Work      string `json:"work"`
	Notes     string `json:"notes,omitempty"`
	Onboarded bool   `json:"onboarded"`

	// TZ is the person's IANA timezone, resolved from the country they pick at
	// onboarding rather than sniffed off the device.
	//
	// This call is the ONLY way a zone reaches the machine, and the guest puts
	// itself on that clock when it starts. A zone that also rode along on
	// ordinary messages would be a second source for one fact, and the one
	// arriving on every request is the one that drifts: a VPN, a second device
	// or a wrong clock would silently re-anchor every schedule the person had
	// booked.
	//
	// Kept in its own file rather than in the rendered profile, because it is
	// machine state and not something an agent reads about the person. A read
	// still reports it: the app has no other way to find out which country it
	// should be showing, and guessing from the device is what this replaced.
	TZ string `json:"tz,omitempty"`
}

// EventsPage is the body of GET /agents/{id}/events?poll=1.
type EventsPage struct {
	Events      []Event `json:"events"`
	LastEventID int     `json:"last_event_id"`
}

// ModelUsage is what one model has cost, in tokens. Split by model because the
// price differs by a factor of ten across the range, so a single token total
// cannot be turned back into money.
type ModelUsage struct {
	Model string `json:"model"`
	Usage
	Turns int64 `json:"turns"`
}

// UsageReport is GET /usage: what this daemon has spent across every agent it
// runs, in tokens. No dollar figure -- the host owns the price table, so rates
// can change without rebuilding a guest image.
type UsageReport struct {
	ByModel        []ModelUsage `json:"by_model"`
	Turns          int64        `json:"turns"`
	LastDurationMS int64        `json:"last_duration_ms,omitempty"`
	LastActivity   time.Time    `json:"last_activity,omitzero"`
}

// Profile is one kind of agent: its role prompt, its model, and what it may do.
// Here rather than in agentd because the host renders a roster from it and must
// not import the daemon, which would pull the model SDK into the host binary.
// Prompt never crosses the wire; it is the one field a client has no use for.
type Profile struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Model       string   `json:"model"`
	Browser     bool     `json:"browser"`
	Tools       []string `json:"tools"`
	Prompt      string   `json:"-"`
}

// Task is a piece of work an agent has open.
type Task struct {
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Dir       string    `json:"dir"`
	StartedAt time.Time `json:"started_at"`
}

// Record is one agent's durable identity: who it is, not whether it is running.
type Record struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Task      *Task     `json:"task,omitempty"`
}

// Schedule is a standing instruction to message an agent at a given time.
//
// Task is the message itself, and it can be short and referential -- "check the
// deploy queue again" -- because it lands in the conversation of the very agent
// that asked for it, which is the whole reason firing into the agent's own inbox
// is simpler than running it in isolation.
type Schedule struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Agent string `json:"agent"`
	Task  string `json:"task"`
	Expr  string `json:"expr"`

	NextRunAt time.Time `json:"next_run_at"`
	LastFired time.Time `json:"last_fired_at,omitzero"`
	Fires     int       `json:"fires"`
	Enabled   bool      `json:"enabled"`
}

// Status is one agent as GET /agents reports it: identity plus what it is doing.
type Status struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	State        string `json:"state"`
	Live         bool   `json:"live"`
	Task         *Task  `json:"task,omitempty"`
	LastEventID  int    `json:"last_event_id"`
	Conversation int    `json:"conversation_bytes"`
}

// Health is GET /health. Ready is constant true rather than a real signal, and
// that is honest: the supervisor is fully built before the listener opens, so
// there is no window in which the daemon answers this un-ready.
type Health struct {
	OK           bool   `json:"ok"`
	Ready        bool   `json:"ready"`
	Agents       int    `json:"agents"`
	Live         int    `json:"live"`
	Working      int    `json:"working"`
	SessionState string `json:"session_state"`
}
