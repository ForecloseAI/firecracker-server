package chat

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
)

// feedOver drives one connection against a live fake guest, with resolve
// standing in for what the control plane says about where that machine lives
// now. Returns everything that reached the client, and whether the stream ended
// by itself rather than being cancelled.
func feedOver(t *testing.T, wait time.Duration,
	resolver func(host string) func() (string, error)) (string, bool) {
	t.Helper()
	oldTick := tick
	tick = 5 * time.Millisecond
	t.Cleanup(func() { tick = oldTick })

	g := &fakeGuest{roster: []agentapi.Status{{ID: "boss", Name: "Boss", Type: "boss"}}}
	srv := httptest.NewServer(g.routes())
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	oldPort := guestPort
	guestPort, _ = strconv.Atoi(port)
	t.Cleanup(func() { guestPort = oldPort })

	f := newFeed("m1", func() string { return "" })
	f.guestIP = host
	f.resolve = resolver(host)

	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("GET", "/v1/stream", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		f.run(r, w, http.NewResponseController(w), agent.New(host, guestPort))
		close(done)
	}()
	select {
	case <-done:
		return w.Body.String(), true
	case <-time.After(wait):
		cancel()
		<-done
		return w.Body.String(), false
	}
}

// The leak this exists to close.
//
// Slot addresses are derived from the slot number, so a deleted machine frees an
// address another account's machine is later given. A connection opened before
// that, polling the address it started with, would forward the new occupant's
// roster and messages to the wrong person. It must notice and stop instead.
func TestFeedStopsWhenAnotherMachineTakesTheAddress(t *testing.T) {
	body, ended := feedOver(t, 200*time.Millisecond,
		func(string) func() (string, error) {
			return func() (string, error) { return "172.16.9.99", nil }
		})
	if !ended {
		t.Fatal("the stream kept polling an address that is no longer its machine")
	}
	if strings.Contains(body, "data:") {
		t.Errorf("frames were forwarded from a machine that is not this account's:\n%s", body)
	}
}

// A machine that is gone ends the connection rather than leaving the app on a
// frozen roster and a heartbeat every 25 seconds with nothing reporting a fault.
func TestFeedStopsWhenTheMachineIsGone(t *testing.T) {
	body, ended := feedOver(t, 200*time.Millisecond,
		func(string) func() (string, error) {
			return func() (string, error) { return "", ErrNoVM }
		})
	if !ended {
		t.Fatal("the stream outlived its machine")
	}
	if strings.Contains(body, "data:") {
		t.Errorf("frames were forwarded after the machine went away:\n%s", body)
	}
}

// While the machine cannot be confirmed the connection sends nothing: quiet is
// recoverable, a misdelivered roster is not. It rides out a brief outage rather
// than dropping the app on the first failed lookup.
func TestFeedSendsNothingWhileUnverifiable(t *testing.T) {
	unresolvable := func(string) func() (string, error) {
		return func() (string, error) { return "", context.DeadlineExceeded }
	}
	// Well inside staleLimit polls, so the connection is still up.
	body, ended := feedOver(t, time.Duration(staleLimit/3)*5*time.Millisecond, unresolvable)
	if ended {
		t.Error("a brief control-plane outage dropped the stream immediately")
	}
	if strings.Contains(body, "data:") {
		t.Errorf("frames were sent without confirming the machine:\n%s", body)
	}
}

// The quiet is bounded, though. A stream that can never confirm its machine
// ends, so the client reconnects and refetches instead of showing a frozen
// roster forever.
func TestFeedGivesUpWhenItNeverResolves(t *testing.T) {
	_, ended := feedOver(t, 2*time.Second, func(string) func() (string, error) {
		return func() (string, error) { return "", context.DeadlineExceeded }
	})
	if !ended {
		t.Error("a permanently unresolvable stream stayed open")
	}
}

// The ordinary case still streams: same machine, same address, frames flow.
func TestFeedRunsWhileTheMachineIsItsOwn(t *testing.T) {
	body, ended := feedOver(t, 200*time.Millisecond, func(host string) func() (string, error) {
		return func() (string, error) { return host, nil }
	})
	if ended {
		t.Fatal("the stream ended against its own healthy machine")
	}
	if !strings.Contains(body, "data:") {
		t.Errorf("no frames reached the client:\n%s", body)
	}
}

// Without a way to confirm which machine is answering, a feed must not stream at
// all -- forgetting the resolver would quietly restore the leak above.
func TestFeedRefusesToStreamWithoutAResolver(t *testing.T) {
	f := newFeed("m1", func() string { return "" })
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		f.run(httptest.NewRequest("GET", "/v1/stream", nil), w, http.NewResponseController(w), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a feed with no resolver started polling anyway")
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing", w.Body.String())
	}
}
