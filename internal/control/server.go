package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// JailController is the interface the server calls into (implemented by engine.Manager).
type JailController interface {
	StartJail(ctx context.Context, name string) error
	StopJail(ctx context.Context, name string) error
	RestartJail(ctx context.Context, name string) error
	JailStatus(name string) (string, error)
	AllJailStatuses() map[string]string
	ConfigFiles(name string, limit int, logFiles bool) ([]string, error)
	ConfigTest(name, filePath string, limit int, returnMatching bool) (totalLines, matchingLines int, matches []string, err error)
	PerfStats() PerfResponse
	StartWhitelist(ctx context.Context, name string) error
	StopWhitelist(ctx context.Context, name string) error
	RestartWhitelist(ctx context.Context, name string) error
	WhitelistStatus(name string) (string, error)
	AllWhitelistStatuses() map[string]string
}

// Server serves the control API over a Unix domain socket.
type Server struct {
	socketPath string
	controller JailController
}

// NewServer creates a new Server.
func NewServer(socketPath string, controller JailController) *Server {
	return &Server{socketPath: socketPath, controller: controller}
}

// Serve starts the HTTP server on the Unix socket. It removes any existing
// socket file before binding, blocks until ctx is cancelled, then shuts down
// gracefully.
func (s *Server) Serve(ctx context.Context) error {
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/perf", s.handlePerf)
	mux.HandleFunc("/v1/jails", s.handleJails)
	mux.HandleFunc("/v1/jails/", s.handleJailAction)
	mux.HandleFunc("/v1/whitelists", s.handleWhitelists)
	mux.HandleFunc("/v1/whitelists/", s.handleWhitelistAction)

	srv := &http.Server{Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handlePerf(w http.ResponseWriter, r *http.Request) {
	slog.Info("control request", "method", r.Method, "path", r.URL.Path)
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, s.controller.PerfStats())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	slog.Info("control request", "method", r.Method, "path", r.URL.Path)
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

func (s *Server) handleJails(w http.ResponseWriter, r *http.Request) {
	slog.Info("control request", "method", r.Method, "path", r.URL.Path)
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	statuses := s.controller.AllJailStatuses()
	resp := ListJailsResponse{Jails: make([]JailStatusResponse, 0, len(statuses))}
	for name, status := range statuses {
		resp.Jails = append(resp.Jails, JailStatusResponse{Name: name, Status: status})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleJailAction handles /v1/jails/{name}/status|start|stop|restart|config/files|config/test
func (s *Server) handleJailAction(w http.ResponseWriter, r *http.Request) {
	slog.Info("control request", "method", r.Method, "path", r.URL.Path)

	// Strip prefix "/v1/jails/" and split on "/" (up to 3 parts).
	trimmed := strings.TrimPrefix(r.URL.Path, "/v1/jails/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "not found"})
		return
	}
	name := parts[0]
	action := parts[1]
	var subaction string
	if len(parts) == 3 {
		subaction = parts[2]
	}

	switch action {
	case "status":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		status, err := s.controller.JailStatus(name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, JailStatusResponse{Name: name, Status: status})

	case "start":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		if err := s.controller.StartJail(r.Context(), name); err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})

	case "stop":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		if err := s.controller.StopJail(r.Context(), name); err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})

	case "restart":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		if err := s.controller.RestartJail(r.Context(), name); err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})

	case "config":
		s.handleJailConfig(w, r, name, subaction)

	default:
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "not found"})
	}
}

// handleJailConfig handles /v1/jails/{name}/config/files and /v1/jails/{name}/config/test
func (s *Server) handleJailConfig(w http.ResponseWriter, r *http.Request, name, subaction string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}

	q := r.URL.Query()
	limit := 10
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}

	switch subaction {
	case "files":
		logFiles := q.Get("log") == "true"
		files, err := s.controller.ConfigFiles(name, limit, logFiles)
		if err != nil {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
			return
		}
		if files == nil {
			files = []string{}
		}
		writeJSON(w, http.StatusOK, ConfigFilesResponse{Files: files, Count: len(files)})

	case "test":
		filePath := q.Get("file")
		if filePath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "missing required query parameter: file"})
			return
		}
		returnMatching := q.Get("matching") == "true"
		total, matching, matches, err := s.controller.ConfigTest(name, filePath, limit, returnMatching)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ConfigTestResponse{
			TotalLines:    total,
			MatchingLines: matching,
			Matches:       matches,
		})

	default:
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "not found"})
	}
}

func (s *Server) handleWhitelists(w http.ResponseWriter, r *http.Request) {
	slog.Info("control request", "method", r.Method, "path", r.URL.Path)
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
		return
	}
	statuses := s.controller.AllWhitelistStatuses()
	resp := ListWhitelistsResponse{Whitelists: make([]WhitelistStatusResponse, 0, len(statuses))}
	for name, status := range statuses {
		resp.Whitelists = append(resp.Whitelists, WhitelistStatusResponse{Name: name, Status: status})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWhitelistAction handles /v1/whitelists/{name}/status|start|stop|restart
func (s *Server) handleWhitelistAction(w http.ResponseWriter, r *http.Request) {
	slog.Info("control request", "method", r.Method, "path", r.URL.Path)

	trimmed := strings.TrimPrefix(r.URL.Path, "/v1/whitelists/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "not found"})
		return
	}
	name := parts[0]
	action := parts[1]

	switch action {
	case "status":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		status, err := s.controller.WhitelistStatus(name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, WhitelistStatusResponse{Name: name, Status: status})

	case "start":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		if err := s.controller.StartWhitelist(r.Context(), name); err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})

	case "stop":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		if err := s.controller.StopWhitelist(r.Context(), name); err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})

	case "restart":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
			return
		}
		if err := s.controller.RestartWhitelist(r.Context(), name); err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})

	default:
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "not found"})
	}
}
