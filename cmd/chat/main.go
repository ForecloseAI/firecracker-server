// Command chat serves the app API and the built-in chat page.
package main

import (
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
	control := chat.NewControl(cfg.ControlURL, cfg.Token)
	caps := chat.NewCaps(cfg.VNCOrigin)
	gw, err := chat.NewVNCGateway(caps, cfg.ControlURL, cfg.Token)
	if err != nil {
		return err
	}
	serve(cfg, chat.NewServer(cfg, control, caps).Routes(), gw.Routes())
	return waitForSignal()
}

// serve starts both listeners in the background.
func serve(cfg chat.Config, app, vnc http.Handler) {
	go listen(cfg.Addr, app, "chat")
	go listen(cfg.VNCAddr, vnc, "vnc")
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
