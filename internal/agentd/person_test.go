package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// Onboarding is only worth collecting if the agents actually read it, and the
// prompt is the only place they can.
func TestWhatWeKnowAboutThePersonReachesTheSystemPrompt(t *testing.T) {
	state := t.TempDir()
	err := WritePerson(state, agentapi.Person{Name: "Naman", Work: "Founder, building this"})
	if err != nil {
		t.Fatal(err)
	}
	got := ComposeSystemPrompt(Profile{Prompt: "role"}, t.TempDir(), state)
	for _, want := range []string{"About the person", "Naman", "Founder"} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt never mentions %q", want)
		}
	}
}

// An agent with no profile yet must get no section at all, rather than a heading
// announcing that we know nothing -- which spends prefix tokens saying only that.
func TestNoProfileAddsNothingToThePrompt(t *testing.T) {
	got := ComposeSystemPrompt(Profile{Prompt: "role"}, t.TempDir(), t.TempDir())
	if strings.Contains(got, "About the person") {
		t.Error("an empty profile still added its heading to the prompt")
	}
}

// Facts are appended, never rewritten: two agents recording something at the same
// time must not lose each other's line, and neither may drop what onboarding
// collected.
func TestRememberingAFactKeepsWhatWasThereBefore(t *testing.T) {
	state := t.TempDir()
	if err := WritePerson(state, agentapi.Person{Name: "Naman"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	if err := AppendAboutPerson(state, "Prefers short replies", "boss", now); err != nil {
		t.Fatal(err)
	}
	if err := AppendAboutPerson(state, "Works in IST", "coder", now); err != nil {
		t.Fatal(err)
	}
	got := ReadPerson(state)
	for _, want := range []string{"Naman", "Prefers short replies", "Works in IST", "boss", "2026-08-26"} {
		if !strings.Contains(got, want) {
			t.Errorf("the profile lost %q:\n%s", want, got)
		}
	}
}

// An empty fact must not put a bullet with nothing after it into every agent's
// prompt.
func TestRememberingNothingWritesNothing(t *testing.T) {
	state := t.TempDir()
	if err := AppendAboutPerson(state, "   ", "boss", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(personPath(state)); !os.IsNotExist(err) {
		t.Error("a blank fact created the profile file")
	}
}

// A filename comes from a phone and is handed to agents who will pass it to
// Bash. It has to be reduced to something that cannot escape the folder or mean
// anything to a shell.
func TestUploadNamesCannotEscapeOrInjectAnything(t *testing.T) {
	cases := map[string]string{
		"../../conversation.json": "conversation.json",
		"/etc/passwd":             "passwd",
		`..\..\windows\thing.txt`: "thing.txt",
		"a b; rm -rf ~.pdf":       "a-b--rm--rf--.pdf",
		"...":                     "file",
		"":                        "file",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.ContainsAny(safeName("../x/../y"), "/\\") {
		t.Error("a separator survived, so the name can still name another folder")
	}
}

// The cap is the only thing standing between a phone and a full 5 GiB overlay,
// and a file written past it must not be left behind.
func TestAnOversizeUploadIsRefusedAndNotKept(t *testing.T) {
	workspace := t.TempDir()
	body := strings.NewReader(strings.Repeat("x", maxUpload+1))
	if _, err := saveUpload(workspace, "big.bin", body, time.Now().UTC()); err == nil {
		t.Fatal("an oversize upload was accepted")
	}
	entries, err := os.ReadDir(uploadsDir(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused upload left %d files behind", len(entries))
	}
}

// The happy path: the file lands in the shared workspace, dated, where every
// agent can already read it.
func TestAnUploadLandsInTheSharedWorkspace(t *testing.T) {
	workspace := t.TempDir()
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	saved, err := saveUpload(workspace, "notes.pdf", strings.NewReader("hello"), now)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "2026-08-26-notes.pdf" {
		t.Errorf("stored as %q", saved.Name)
	}
	if filepath.Dir(saved.Path) != uploadsDir(workspace) {
		t.Errorf("landed at %q, outside the uploads folder", saved.Path)
	}
	if saved.Size != 5 {
		t.Errorf("recorded %d bytes, want 5", saved.Size)
	}
	body, err := os.ReadFile(saved.Path)
	if err != nil || string(body) != "hello" {
		t.Errorf("the file on disk is %q (%v)", body, err)
	}
}

// The path is framing for the model, not something the person typed. Pasting it
// into the message itself would put a Linux path in the chat bubble on every
// reload, where the app means to draw an attachment card.
func TestAnAttachedFileIsToldToTheModelButNotToTheTranscript(t *testing.T) {
	file := &agentapi.File{Name: "notes.pdf", Path: "/home/agent/workspace/uploads/notes.pdf"}
	framed := frame(inbound{text: "what is in this?", file: file})
	if !strings.Contains(framed, file.Path) {
		t.Error("the model was never told where the file is")
	}
	if !strings.Contains(framed, "what is in this?") {
		t.Error("framing lost the question")
	}
	if got := (inbound{text: "what is in this?", file: file}).text; got != "what is in this?" {
		t.Errorf("the message itself became %q", got)
	}
}

// A file with no message is a valid thing to send, and must still reach the
// model as something it can act on.
func TestAFileWithNoMessageStillNamesThePath(t *testing.T) {
	file := &agentapi.File{Name: "x.csv", Path: "/home/agent/workspace/uploads/x.csv"}
	framed := frame(inbound{file: file})
	if !strings.Contains(framed, file.Path) || strings.HasPrefix(framed, "\n") {
		t.Errorf("a bare attachment framed as %q", framed)
	}
}

// Changing where you live must not cost you your name.
//
// The settings screen sends a zone and nothing else, because GET /person does
// not hand back the name and work for it to echo. WritePerson replaces the
// file, so a zone-only body that reached it would render an empty profile over
// the real one and every agent would start the next turn not knowing who they
// work for.
func TestAZoneOnlyUpdateKeepsTheProfile(t *testing.T) {
	sup := newTestSupervisor(t)
	srv := NewServer(sup)

	if w := do(t, srv, "PUT", "/person",
		`{"name":"Naman","work":"Founder","tz":"Asia/Kolkata"}`); w.Code != 204 {
		t.Fatalf("onboarding put = %d, want 204: %s", w.Code, w.Body)
	}
	if w := do(t, srv, "PUT", "/person", `{"tz":"Europe/Berlin"}`); w.Code != 204 {
		t.Fatalf("zone-only put = %d, want 204: %s", w.Code, w.Body)
	}

	if got := ReadPerson(sup.stateDir); !strings.Contains(got, "Naman") {
		t.Errorf("profile after a zone change = %q, want it to still name them", got)
	}
	if got := loadZone(sup.stateDir).String(); got != "Europe/Berlin" {
		t.Errorf("zone = %q, want Europe/Berlin: the change still has to land", got)
	}
}
