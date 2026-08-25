package agentd

import (
	"context"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// Inputs for the acting tools. As above: no commas in any description.
type clickInput struct {
	UID string `json:"uid" jsonschema:"required,description=The uid of an element from the latest snapshot"`
}

type fillInput struct {
	UID   string `json:"uid" jsonschema:"required,description=The uid of the field from the latest snapshot"`
	Value string `json:"value" jsonschema:"required,description=Text to put in the field - it replaces whatever is there"`
}

type pressKeyInput struct {
	Key string `json:"key" jsonschema:"required,description=A key such as Enter or Tab or Escape - or a combination such as Control+A"`
}

type waitForInput struct {
	Text      []string `json:"text" jsonschema:"required,description=Wait until any one of these strings appears on the page"`
	TimeoutMS int      `json:"timeout_ms" jsonschema:"description=How long to wait in milliseconds - defaults to 5000 and caps at 30000"`
}

type dialogInput struct {
	Action string `json:"action" jsonschema:"required,description=Either accept or dismiss"`
	Text   string `json:"text" jsonschema:"description=Text to type into a prompt dialog before accepting it"`
}

// clickTool presses an element the model picked out of a snapshot.
func clickTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[clickInput](
		"click", "Click an element using a uid from the latest snapshot.",
		func(ctx context.Context, in clickInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, why := usePage(ctx, d)
			if why != "" {
				return toolText(why), nil
			}
			return toolText(runClick(ctx, b, in.UID)), nil
		})
}

// runClick clicks and says what it clicked.
func runClick(ctx context.Context, b browser, uid string) string {
	before := b.SnapshotGen()
	t, err := b.Click(ctx, uid)
	if err != nil {
		return err.Error()
	}
	return "clicked " + t.Label() + afterAction(b, before)
}

// fillTool types into a field.
func fillTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[fillInput](
		"fill",
		"Type into a text field using a uid from the latest snapshot. Use click for checkboxes and radio buttons.",
		func(ctx context.Context, in fillInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, why := usePage(ctx, d)
			if why != "" {
				return toolText(why), nil
			}
			return toolText(runFill(ctx, b, in)), nil
		})
}

// runFill types and reports what the field actually reads afterwards.
//
// The mismatch branch is the point of the readback: a readonly or
// length-clamped field accepts every command and keeps its old value, and
// without this the tool would report a fill that never happened.
func runFill(ctx context.Context, b browser, in fillInput) string {
	before := b.SnapshotGen()
	t, got, err := b.Fill(ctx, in.UID, in.Value)
	if err != nil {
		return err.Error()
	}
	if got != in.Value {
		return "typed into " + t.Label() + " but it now reads " + quote(got) +
			" rather than " + quote(in.Value) + " - it may have a length limit or reformat what it is given."
	}
	return "typed " + quote(in.Value) + " into " + t.Label() + insertTextCaveat + afterAction(b, before)
}

// insertTextCaveat names what typing this way does not do.
//
// Text is inserted through Blink's editing pipeline, so input and beforeinput
// fire and a React or Vue field updates -- but there is no keydown or keyup, so
// input masks and type-ahead menus wired to keystrokes do not react. Saying so
// gives the model a next move instead of a mystery.
const insertTextCaveat = "\nIf the page did not react the way you expected some sites need real keystrokes - try press_key."

// pressKeyTool sends a keystroke to whatever has focus.
func pressKeyTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[pressKeyInput](
		"press_key",
		"Press a key on whatever is focused. Use it to submit a form with Enter or to move focus with Tab.",
		func(ctx context.Context, in pressKeyInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, why := usePage(ctx, d)
			if why != "" {
				return toolText(why), nil
			}
			return toolText(runPressKey(ctx, b, in.Key)), nil
		})
}

// runPressKey presses one key or combination.
func runPressKey(ctx context.Context, b browser, key string) string {
	before := b.SnapshotGen()
	if err := b.PressKey(ctx, key); err != nil {
		return err.Error()
	}
	return "pressed " + key + afterAction(b, before)
}

// waitForTool waits for text to appear, which is what to do after submitting
// rather than snapshotting straight away and reading the old page.
func waitForTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[waitForInput](
		"wait_for",
		"Wait until some text appears on the page. Text inside an embedded frame will not be found.",
		func(ctx context.Context, in waitForInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, why := usePage(ctx, d)
			if why != "" {
				return toolText(why), nil
			}
			got, err := b.WaitFor(ctx, in.Text, time.Duration(in.TimeoutMS)*time.Millisecond)
			if err != nil {
				return toolText(err.Error()), nil
			}
			return toolText("found " + quote(got) + " on the page\nTake a snapshot to see it."), nil
		})
}

// dialogTool answers a JavaScript dialog.
//
// It goes through reachChrome rather than usePage, because usePage refuses
// while a dialog is open and this is the only tool that can clear one.
func dialogTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[dialogInput](
		"handle_dialog",
		"Accept or dismiss a dialog the page has opened. Nothing else on the page works until you do.",
		func(ctx context.Context, in dialogInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, why := reachChrome(ctx, d)
			if why != "" {
				return toolText(why), nil
			}
			return toolText(runDialog(ctx, b, in)), nil
		})
}

// runDialog accepts or dismisses whatever Chrome is showing.
func runDialog(ctx context.Context, b browser, in dialogInput) string {
	if b.Blocked() == "" {
		return "there is no dialog open"
	}
	accept := strings.EqualFold(in.Action, "accept")
	if err := b.HandleDialog(ctx, accept, in.Text); err != nil {
		return "could not answer the dialog: " + err.Error()
	}
	return map[bool]string{true: "accepted the dialog", false: "dismissed the dialog"}[accept] +
		"\nTake a snapshot to see what the page is showing now."
}

// afterAction tells the model what the page did, in the two forms that differ.
//
// A generic "take a new snapshot if anything changed" on every single result is
// noise the model learns to skip past. A navigation it can be told about
// outright is not -- and when the page did not move, saying so keeps a
// fill-fill-click run on one form from re-snapshotting between every step.
func afterAction(b browser, before int) string {
	if b.SnapshotGen() != before {
		return "\nThe page navigated. Uids from that snapshot are gone - take a new one before acting again."
	}
	return "\nIf the page changed shape take a new snapshot before using another uid."
}

// quote renders a value the way it will read back to the model.
func quote(s string) string { return `"` + s + `"` }
