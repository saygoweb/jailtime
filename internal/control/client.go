package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
)

// Client sends requests to jailtimed over the Unix socket.
type Client struct {
	socketPath string
	httpClient *http.Client
}

// NewClient creates a Client that communicates over the given Unix socket path.
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{Transport: transport},
	}
}

func (c *Client) get(path string, out any) error {
	resp, err := c.httpClient.Get("http://jailtimed" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("server error: %s", e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) post(path string) error {
	resp, err := c.httpClient.Post("http://jailtimed"+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("server error: %s", e.Error)
	}
	return nil
}

// Health calls GET /v1/health.
func (c *Client) Health() (*HealthResponse, error) {
	var resp HealthResponse
	if err := c.get("/v1/health", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListJails calls GET /v1/jails.
func (c *Client) ListJails() (*ListJailsResponse, error) {
	var resp ListJailsResponse
	if err := c.get("/v1/jails", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// JailStatus calls GET /v1/jails/{name}/status.
func (c *Client) JailStatus(name string) (*JailStatusResponse, error) {
	var resp JailStatusResponse
	if err := c.get("/v1/jails/"+name+"/status", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StartJail calls POST /v1/jails/{name}/start.
func (c *Client) StartJail(name string) error {
	return c.post("/v1/jails/" + name + "/start")
}

// StopJail calls POST /v1/jails/{name}/stop.
func (c *Client) StopJail(name string) error {
	return c.post("/v1/jails/" + name + "/stop")
}

// RestartJail calls POST /v1/jails/{name}/restart.
func (c *Client) RestartJail(name string) error {
	return c.post("/v1/jails/" + name + "/restart")
}
