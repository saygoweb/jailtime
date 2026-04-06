package control

// JailStatusResponse represents one jail's status in API responses.
type JailStatusResponse struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ListJailsResponse is returned by GET /v1/jails.
type ListJailsResponse struct {
	Jails []JailStatusResponse `json:"jails"`
}

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Status string `json:"status"`
}

// ErrorResponse wraps error messages.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ConfigFilesResponse is returned by GET /v1/jails/{name}/config/files.
type ConfigFilesResponse struct {
	Files []string `json:"files"`
	Count int      `json:"count"`
}

// ConfigTestResponse is returned by GET /v1/jails/{name}/config/test.
type ConfigTestResponse struct {
	TotalLines    int      `json:"total_lines"`
	MatchingLines int      `json:"matching_lines"`
	Matches       []string `json:"matches,omitempty"`
}

// PerfResponse is returned by GET /v1/perf.
type PerfResponse struct {
	CurrentLatencyMs float64 `json:"current_latency_ms"`
	CurrentDelayMs   float64 `json:"current_delay_ms"`
	AvgExecTimeMs    float64 `json:"avg_exec_time_ms"`
	AvgCPUPercent    float64 `json:"avg_cpu_percent"`
	WindowSize       int     `json:"window_size"`
}
