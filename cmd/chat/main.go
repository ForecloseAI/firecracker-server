// Command chat serves the app API and the built-in chat page.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"cracked/internal/chat"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run wires the service up and serves until a signal arrives.
func run() error {
	cfg, err := chat.LoadConfig()
	if err != nil {
		return err
	}
	// Fetched at startup, not lazily: a wrong SUPABASE_URL should stop the
	// service coming up rather than tell every user they are unauthorized.
	auth, err := chat.NewVerifier(context.Background(), cfg.SupabaseURL)
	if err != nil {
		return err
	}
	control := chat.NewControl(cfg.ControlURL, cfg.Token)
	caps := chat.NewCaps(cfg.VNCOrigin)
	gw, err := chat.NewVNCGateway(caps, cfg.ControlURL, cfg.Token)
	if err != nil {
		return err
	}
	srv := chat.NewServer(cfg, control, caps, auth)
	go listen(cfg.Addr, srv.Routes(), "chat")
	go listen(cfg.VNCAddr, gw.Routes(), "vnc")
	// The one listener a guest can reach. Started only when a provider is
	// configured, so a deployment without one opens no guest-facing port at all.
	if apps := srv.AppsRoutes(); apps != nil {
		go listen(cfg.AppsAddr, apps, "apps")
	}
	return waitForSignal()
}

// listen runs one HTTP server. WriteTimeout is deliberately unset: it is an
// absolute deadline and would truncate every SSE stream.
func listen(addr string, h http.Handler, name string) {
	log.Printf("%s listening on %s", name, addr)
	srv := &http.Server{Addr: addr, Handler: h}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("%s listen: %v", name, err)
	}
}

// waitForSignal blocks until the service is asked to stop.
func waitForSignal() error {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("received %s, shutting down", <-ch)
	return nil
}
