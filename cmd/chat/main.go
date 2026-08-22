// Command cracked-chat serves the browser-facing chat UI for the VM agents.
// It runs two listeners on two origins: the chat app, and a VNC gateway that
// renders untrusted guest HTML and therefore must never share an origin with
// the session cookie.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"cracked/internal/chat"
)

func main() {
	hashUser := flag.String("hashpw", "", "print a users-file line for this username, password on stdin")
	flag.Parse()
	if *hashUser != "" {
		if err := runHashPW(*hashUser); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// runHashPW reads a password from stdin and prints its users-file line.
func runHashPW(user string) error {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return errors.New("password must not be empty")
	}
	fmt.Println(chat.HashPassword(user, password))
	return nil
}

// run wires the service up and serves until a signal arrives.
func run() error {
	cfg, err := chat.LoadConfig()
	if err != nil {
		return err
	}
	auth, err := loadAuth(cfg)
	if err != nil {
		return err
	}
	control := chat.NewControl(cfg.ControlURL, cfg.Token)
	caps := chat.NewCaps(cfg.VNCOrigin)
	gw, err := chat.NewVNCGateway(caps, cfg.ControlURL, cfg.Token)
	if err != nil {
		return err
	}
	serve(cfg, chat.NewServer(cfg, auth, control, caps).Routes(), gw.Routes())
	go reloadOnHUP(cfg, auth)
	return waitForSignal()
}

// loadAuth reads the credential file.
func loadAuth(cfg chat.Config) (*chat.Auth, error) {
	creds, err := chat.LoadCreds(cfg.UsersFile)
	if err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	return chat.NewAuth(creds), nil
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

// reloadOnHUP re-reads the users file so adding a login does not drop streams.
func reloadOnHUP(cfg chat.Config, auth *chat.Auth) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	for range ch {
		creds, err := chat.LoadCreds(cfg.UsersFile)
		if err != nil {
			log.Printf("reload users: %v", err)
			continue
		}
		auth.SetCreds(creds)
		log.Print("users reloaded")
	}
}

// waitForSignal blocks until the service is asked to stop.
func waitForSignal() error {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("received %s, shutting down", <-ch)
	return nil
}
