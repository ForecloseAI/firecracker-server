// Package fc speaks the firecracker HTTP API over its unix domain socket.
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client is a firecracker API socket client for one microVM.
type Client struct {
	http *http.Client
}

// New builds a client whose transport dials the given unix socket path.
func New(sockPath string) *Client {
	d := &net.Dialer{Timeout: 2 * time.Second}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", sockPath)
	}}
	return &Client{http: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
}

// InstanceInfo is the payload of GET /, used as the authoritative VM state.
type InstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	VMMVersion string `json:"vmm_version"`
	AppName    string `json:"app_name"`
}

// Describe reads GET / and reports the running microVM's state.
func (c *Client) Describe() (*InstanceInfo, error) {
	var info InstanceInfo
	if err := c.do(http.MethodGet, "/", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// SetVMState issues PATCH /vm with "Paused" or "Resumed".
func (c *Client) SetVMState(state string) error {
	return c.do(http.MethodPatch, "/vm", map[string]string{"state": state}, nil)
}

// Action issues PUT /actions, e.g. SendCtrlAltDel.
func (c *Client) Action(action string) error {
	return c.do(http.MethodPut, "/actions", map[string]string{"action_type": action}, nil)
}

// do performs one API call, encoding body and decoding into out when non-nil.
func (c *Client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://localhost"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.send(req, out)
}

// send executes the request and decodes a success response.
func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
