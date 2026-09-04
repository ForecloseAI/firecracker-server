package chat

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// guestRoutes is the one handler tree a guest can reach: connected-apps tickets
// under /apps/, the model credential under /v1/. Either broker may be absent,
// and an unmatched path is a bare 404.
//
// Its own listener and its own mux, deliberately: it is the one surface an
// untrusted guest can reach, and mounting it on the mux that serves the app's
// /v1 and the operator page would put it one routing mistake away from both.
// It gets its own logging for the same reason, and never the app's: the
// bodies are prompts.
func guestRoutes(apps *AppsGateway, llm *LLMGateway) http.Handler {
	mux := http.NewServeMux()
	if apps != nil {
		mux.HandleFunc(appsGatewayPrefix, apps.serve)
	}
	if llm != nil {
		mux.HandleFunc(llmGatewayPrefix, llm.serve)
	}
	return guestLogged(mux)
}

// guestLogged writes one line per guest request: who, what, how it went and
// how long it took. Never the body -- prompts are the person's own words, and
// the journal is what gets read aloud when debugging.
func guestLogged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("guest broker: %s %s %s -> %d in %s", r.RemoteAddr, r.Method,
			guestPath(r.URL.Path), sw.code, time.Since(started).Round(time.Millisecond))
	})
}

// guestPath is a request path as the journal may see it. An apps path carries
// the guest's ticket, which is half of what lets it through, so only the prefix
// is kept; the model paths name nothing and are kept whole.
func guestPath(path string) string {
	if strings.HasPrefix(path, appsGatewayPrefix) {
		return appsGatewayPrefix + "<ticket>"
	}
	return path
}

// keepHeaders copies only the allowed headers out of a request's set, so what
// comes back is exactly the allow list and nothing the guest added.
func keepHeaders(in http.Header, allow []string) http.Header {
	kept := make(http.Header, len(allow))
	for _, h := range allow {
		if v := in.Values(h); len(v) > 0 {
			kept[http.CanonicalHeaderKey(h)] = v
		}
	}
	return kept
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
}

// WriteHeader records the status and passes it on.
func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the flusher underneath, which is
// what keeps a streamed response flushing through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
