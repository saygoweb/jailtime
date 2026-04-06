package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

// Perf calls GET /v1/perf.
func (c *Client) Perf() (*PerfResponse, error) {
	var resp PerfResponse
	if err := c.get("/v1/perf", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
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

// ConfigFiles calls GET /v1/jails/{name}/config/files.
func (c *Client) ConfigFiles(name string, limit int, logFiles bool) (*ConfigFilesResponse, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if logFiles {
		q.Set("log", "true")
	}
	var resp ConfigFilesResponse
	if err := c.get("/v1/jails/"+name+"/config/files?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ConfigTest calls GET /v1/jails/{name}/config/test.
func (c *Client) ConfigTest(name, filePath string, limit int, returnMatching bool) (*ConfigTestResponse, error) {
	q := url.Values{}
	q.Set("file", filePath)
	q.Set("limit", strconv.Itoa(limit))
	if returnMatching {
		q.Set("matching", "true")
	}
	var resp ConfigTestResponse
	if err := c.get("/v1/jails/"+name+"/config/test?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
