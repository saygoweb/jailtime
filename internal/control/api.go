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
	TargetLatencyMs float64 `json:"target_latency_ms"`
	LatencyMs       float64 `json:"latency_ms"`
	ExecutionMs     float64 `json:"execution_ms"`
	SleepMs         float64 `json:"sleep_ms"`
	LinesProcessed  int     `json:"lines_processed"`
	CPUPercent      float64 `json:"cpu_percent"`
}

// WhitelistStatusResponse represents one whitelist's status.
type WhitelistStatusResponse struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ListWhitelistsResponse is returned by GET /v1/whitelists.
type ListWhitelistsResponse struct {
	Whitelists []WhitelistStatusResponse `json:"whitelists"`
}
