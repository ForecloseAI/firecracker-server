package chat

import (
	_ "embed"
	"net/http"
)

//go:embed static/connected.html
var connectedPage []byte

// connectedPath is where a person lands after approving an app with a provider.
//
// Under /v1 so it sits with the rest of the app's surface, but deliberately
// OUTSIDE apiGuard: the browser arriving here has come from a sign-in page and
// carries no access token. Answering 401 would show someone an error at the end
// of a flow that actually succeeded.
const connectedPath = "/v1/apps/connected"

// connected serves the page that hands a person back to the app.
//
// It is safe to serve unauthenticated because it does nothing: no state is read
// or written, no parameter is used, and nothing about who is asking changes the
// bytes. Whether the connection actually worked is settled by the agent retrying
// the call, not by anything here -- so there is no outcome to report and no
// reason to trust what the query string claims.
func (s *Server) connected(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// This origin also serves the operator console. The page is static and has
	// no scripts beyond its own, so pinning it shut costs nothing and stops it
	// ever becoming a way to run something here.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'")
	// The content type and no-store come from servePage, which is what every
	// other embedded page here is served through.
	servePage(w, connectedPage)
}
