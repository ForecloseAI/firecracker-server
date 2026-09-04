package chat

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// bodyCap is how much of a request or response reaches the log. Enough to see
// what an app sent and what came back; short enough that one roster fetch does
// not bury the line after it.
const bodyCap = 400

// captureCap bounds what is read for logging. The handler still gets the whole
// body -- this only limits what is remembered to print.
const captureCap = 8 << 10

// secrets are redacted before anything is written. A token arrives in the query
// string because SSE cannot set headers, and a body may carry one too. Without
// this they would sit in the journal in the clear. There is no sign-in here --
// identity lives in Supabase -- so the password pattern is a backstop for a body
// that carries one anyway, not a route this service serves.
var secrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(token=)[^&\s"]+`),
	regexp.MustCompile(`(?i)("password"\s*:\s*)"[^"]*"`),
	regexp.MustCompile(`(?i)("token"\s*:\s*)"[^"]*"`),
}

// redact removes anything that must not be logged.
func redact(s string) string {
	for _, re := range secrets {
		s = re.ReplaceAllString(s, "${1}REDACTED")
	}
	return s
}

// logged records every request and its response, so an integration problem is
// a line you can point at rather than a report with nothing behind it.
//
// Bodies are included unless CHAT_LOG_BODIES=0, which is the switch to reach for
// once real people are using this: message text is their content, not ours.
//
// It also verifies the caller, because the log wants a name on every line and
// the guards want an identity: doing it here means one verification per request
// rather than one per wrapper.
func logged(next http.Handler, bodies bool, auth *Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// The one place a token is verified. This wrapper is outermost, so every
		// guard below can read the answer off the context instead of checking a
		// signature again on the same request.
		user := ""
		if id, ok := auth.identify(r); ok {
			user = id.Email
			r = withToken(withIdentity(r, id), requestToken(r))
		}
		req := ""
		if bodies {
			req = captureRequest(r)
		}
		lw := &logWriter{statusWriter: statusWriter{ResponseWriter: w, code: http.StatusOK},
			keep: bodies, user: user, req: r}
		next.ServeHTTP(lw, r)
		lw.report(start, user, req)
	})
}

// captureRequest reads the body for the log and puts it back for the handler.
func captureRequest(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, captureCap))
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
	return string(head)
}

// logWriter remembers what a handler answered.
type logWriter struct {
	statusWriter
	body   bytes.Buffer
	keep   bool
	sse    bool
	opened bool
	user   string
	req    *http.Request
}

// WriteHeader records the status, and announces a stream as it opens rather
// than only when it ends -- a stream that stays open for an hour would
// otherwise be invisible for an hour.
func (w *logWriter) WriteHeader(code int) {
	w.sse = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	if w.sse && !w.opened {
		w.opened = true
		log.Printf("SSE open  %s %s user=%s", w.req.Method, redact(w.req.URL.RequestURI()), or(w.user))
	}
	w.statusWriter.WriteHeader(code)
}

// Write copies what is sent, except for a stream, which never ends.
func (w *logWriter) Write(b []byte) (int, error) {
	if w.keep && !w.sse && w.body.Len() < bodyCap {
		w.body.Write(b[:min(len(b), bodyCap-w.body.Len())])
	}
	return w.ResponseWriter.Write(b)
}

// Flush passes through, so a stream is not buffered by this wrapper.
func (w *logWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// report writes the one line that describes the whole exchange.
func (w *logWriter) report(start time.Time, user, req string) {
	took := time.Since(start).Round(time.Millisecond)
	uri := redact(w.req.URL.RequestURI())
	if w.sse {
		log.Printf("SSE close %s %s %s user=%s", w.req.Method, uri, took, or(user))
		return
	}
	log.Printf("%d %s %s %s user=%s%s%s", w.code, w.req.Method, uri, took, or(user),
		field(" req=", req), field(" res=", w.body.String()))
}

// field renders one body for the log, trimmed to a single readable line.
func field(label, body string) string {
	body = strings.TrimSpace(redact(body))
	if body == "" {
		return ""
	}
	body = strings.Join(strings.Fields(body), " ")
	if len(body) > bodyCap {
		body = body[:bodyCap] + "…"
	}
	return label + body
}

// or names an unauthenticated caller rather than leaving the field blank.
func or(user string) string {
	if user == "" {
		return "-"
	}
	return user
}
