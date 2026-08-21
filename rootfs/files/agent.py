#!/usr/bin/env python3
"""Placeholder agent runtime.

Exposes the health endpoint the control plane polls to decide a VM has booted,
plus a minimal exec endpoint used by the verification plan. It also starts the
window manager and Chrome on display :0 -- the same display x11vnc exports, so
a human viewer shares the agent's session and can take over at any time.
"""
import json
import subprocess
import shlex
from http.server import BaseHTTPRequestHandler, HTTPServer

WORKSPACE = "/home/agent/workspace"
CHROME_FLAGS = [
    # The microVM is the security boundary; Chrome's setuid sandbox needs a
    # SUID bit that docker export drops plus userns the CI kernel may not have.
    "--no-sandbox",
    "--disable-dev-shm-usage",
    "--user-data-dir=/home/agent/.config/google-chrome",
    "--start-maximized",
    "about:blank",
]


def start_desktop():
    """Launch the window manager and browser onto the shared display :0."""
    subprocess.Popen(["openbox"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.Popen(
        ["google-chrome"] + CHROME_FLAGS,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


def run_command(cmd):
    """Run a shell command and capture its output for the exec endpoint."""
    proc = subprocess.run(
        cmd, shell=True, capture_output=True, text=True, timeout=60
    )
    return {
        "exit_code": proc.returncode,
        "stdout": proc.stdout,
        "stderr": proc.stderr,
    }


class Handler(BaseHTTPRequestHandler):
    """Minimal JSON HTTP surface for the control plane and verification tests."""

    def _reply(self, status, payload):
        """Send one JSON response."""
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        """Answer health checks; anything else is a 404."""
        if self.path.rstrip("/") in ("/health", ""):
            self._reply(200, {"ok": True})
        else:
            self._reply(404, {"error": "not_found"})

    def do_POST(self):
        """Run a command supplied as {"cmd": "..."}."""
        if self.path.rstrip("/") != "/exec":
            self._reply(404, {"error": "not_found"})
            return
        length = int(self.headers.get("Content-Length", 0))
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
            self._reply(200, run_command(req["cmd"]))
        except Exception as exc:  # surfaced to the caller, never silently dropped
            self._reply(400, {"error": str(exc)})

    def log_message(self, fmt, *args):
        """Silence per-request stderr noise; the guest console is size-capped."""
        return


def main():
    """Start the desktop, then serve until the VM stops."""
    start_desktop()
    HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()


if __name__ == "__main__":
    main()
