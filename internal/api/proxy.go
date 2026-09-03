package api

import (
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"cracked/internal/vm"
)

// handleProxy forwards a subtree to one guest port, rejecting VMs that are not
// running. httputil.ReverseProxy upgrades WebSockets transparently.
func (s *Server) handleProxy(port int, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		v, err := s.reg.Get(id)
		if err != nil {
			writeVMErr(w, err)
			return
		}
		if st := s.reg.Snapshot(v); st != vm.StateRunning {
			writeErr(w, http.StatusConflict,
				apiError{"conflict", "vm is " + string(st) + ", not running", ""})
			return
		}
		proxyTo(v.GuestIP, port, "/vms/"+id+"/"+kind).ServeHTTP(w, r)
	}
}

// proxyTo builds a WebSocket-capable reverse proxy to one guest port,
// stripping the /vms/{id}/{kind} prefix from the forwarded path.
func proxyTo(guestIP string, port int, prefix string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(guestIP, strconv.Itoa(port))}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host = target.Scheme, target.Host
			r.Out.URL.Path = trimPrefix(r.In.URL.Path, prefix)
			// The guest is untrusted: never forward control-plane credentials.
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("Cookie")
		},
		FlushInterval: -1, // immediate flush, required for VNC framebuffer streaming
		ErrorHandler:  proxyError,
	}
}

// trimPrefix strips the mount prefix, always leaving a rooted path.
func trimPrefix(path, prefix string) string {
	out := strings.TrimPrefix(path, prefix)
	if out == "" || out[0] != '/' {
		out = "/" + out
	}
	return out
}

// proxyError renders upstream failures in the standard error shape.
func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("proxy %s: %v", r.URL.Path, err)
	writeErr(w, http.StatusBadGateway, apiError{"vm_unreachable", err.Error(), ""})
}
