package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"cracked/internal/vm"
)

// Control talks to the cracked control plane. The bearer token lives here and
// is sent as a HEADER only: guard() echoes a token into a cookie when it
// arrives via ?token=, so a query-string call could leak a fleet credential
// into a Set-Cookie. A header sets nothing.
type Control struct {
	base  string
	token string
	http  *http.Client
}

// NewControl builds a control-plane client.
func NewControl(baseURL, token string) *Control {
	return &Control{base: baseURL, token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// ErrNoVM means the control plane has no VM by that name.
var ErrNoVM = fmt.Errorf("no such vm")

// VM looks up one VM. ErrNoVM distinguishes a typo from an outage, which is
// what lets the page stop retrying instead of spinning forever.
func (c *Control) VM(id string) (vm.View, error) {
	var out vm.View
	err := c.get("/vms/"+id, &out)
	return out, err
}

// vmList is the shape of GET /vms.
type vmList struct {
	VMs []vm.View `json:"vms"`
}

// List returns every VM the fleet knows about.
func (c *Control) List() ([]vm.View, error) {
	var out vmList
	err := c.get("/vms", &out)
	return out.VMs, err
}

// Resume unpauses a VM so a chat can continue against it.
func (c *Control) Resume(id string) error {
	req, err := http.NewRequest(http.MethodPost, c.base+"/vms/"+id+"/resume", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return statusError(resp, "/vms/"+id+"/resume")
}

// get performs one authenticated read.
func (c *Control) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := statusError(resp, path); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// statusError maps a response status onto an error, keeping 404 distinct.
func statusError(resp *http.Response, path string) error {
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoVM
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("control %s: %s", path, resp.Status)
	}
	return nil
}
