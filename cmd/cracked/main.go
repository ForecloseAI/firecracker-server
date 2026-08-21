// Command cracked is the Firecracker microVM control plane for one EC2 host.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cracked/internal/api"
	"cracked/internal/vm"
)

// drainBudget bounds how long shutdown waits for VMs to stop gracefully.
const drainBudget = 30 * time.Second

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	base := flag.String("base", envOr("CRACKED_BASE", "/var/lib/cracked"), "state directory")
	user := flag.String("tap-user", envOr("CRACKED_USER", "cracked"), "tap device owner")
	flag.Parse()

	token := os.Getenv("CRACKED_TOKEN")
	if token == "" {
		log.Fatal("CRACKED_TOKEN must be set")
	}
	reg := vm.NewRegistry(*base, *user)
	reg.Sweep()
	serve(*addr, reg, token)
}

// envOr reads an environment variable with a fallback.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// serve runs the HTTP server until a signal arrives, then drains every VM.
func serve(addr string, reg *vm.Registry, token string) {
	srv := &http.Server{Addr: addr, Handler: api.New(reg, token).Routes()}
	go func() {
		log.Printf("cracked listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	waitForSignal()
	shutdown(srv, reg)
}

// waitForSignal blocks until SIGINT or SIGTERM.
func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("received %s, shutting down", <-ch)
}

// shutdown stops accepting requests, then tears down every running VM.
func shutdown(srv *http.Server, reg *vm.Registry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	reg.DrainAll(drainBudget)
	log.Print("all VMs drained")
}
