package sessions

import "time"

type Usage struct {
	Reported []string `json:"reported_fields,omitempty"`
	Input    int64    `json:"input_tokens"`
	Output   int64    `json:"output_tokens"`
	Total    int64    `json:"total_tokens"`
	Cached   int64    `json:"cached_input_tokens"`
	Known    bool     `json:"known"`
}

type Failure struct {
	Layer   string `json:"layer,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type Attempt struct {
	Timing                    *Timing             `json:"timing,omitempty"`
	Number                    int                 `json:"number"`
	AccountID                 string              `json:"account_id"`
	Status                    int                 `json:"status"`
	StartedAt                 time.Time           `json:"started_at"`
	DurationMS                int64               `json:"duration_ms"`
	UpstreamRequestID         string              `json:"upstream_request_id,omitempty"`
	Retry                     bool                `json:"retry"`
	Usage                     Usage               `json:"usage"`
	Failure                   Failure             `json:"failure"`
	RequestOutputLimits       map[string]int64    `json:"request_output_limits,omitempty"`
	RateLimitHeaders          map[string][]string `json:"rate_limit_headers,omitempty"`
	RateLimitHeadersTruncated bool                `json:"rate_limit_headers_truncated,omitempty"`
}

type Body struct {
	ContentType string            `json:"content_type"`
	Bytes       int64             `json:"bytes"`
	Complete    bool              `json:"complete"`
	Error       string            `json:"error,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type Record struct {
	Purpose         string       `json:"purpose,omitempty"`
	Title           *Title       `json:"title,omitempty"`
	UserMessages    []string     `json:"user_message_hashes,omitempty"`
	Identity        Identity     `json:"identity"`
	HistoryHash     string       `json:"history_hash,omitempty"`
	ToolCallIDs     []string     `json:"tool_call_ids,omitempty"`
	TurnStateHash   string       `json:"turn_state_hash,omitempty"`
	Performance     Performance  `json:"performance"`
	TokenDetails    TokenDetails `json:"token_details"`
	ID              string       `json:"id"`
	SessionID       string       `json:"session_id"`
	StartedAt       time.Time    `json:"started_at"`
	FinishedAt      time.Time    `json:"finished_at"`
	Method          string       `json:"method"`
	Path            string       `json:"path"`
	Query           string       `json:"query,omitempty"`
	Provider        string       `json:"provider"`
	Group           string       `json:"group,omitempty"`
	Model           string       `json:"model"`
	AccountID       string       `json:"account_id,omitempty"`
	ResponseID      string       `json:"response_id,omitempty"`
	Status          int          `json:"status"`
	Outcome         string       `json:"outcome"`
	DurationMS      int64        `json:"duration_ms"`
	PoolWaitMS      int64        `json:"pool_wait_ms,omitempty"`
	PoolWaits       int          `json:"pool_waits,omitempty"`
	Request         Body         `json:"request"`
	Response        Body         `json:"response"`
	Usage           Usage        `json:"usage"`
	UsageComplete   bool         `json:"usage_complete"`
	UnknownAttempts int          `json:"attempts_without_usage"`
	Attempts        []Attempt    `json:"attempts"`
	Failure         Failure      `json:"failure"`
	CaptureError    string       `json:"capture_error,omitempty"`
}

type Summary struct {
	NameInfo             NameInfo          `json:"name_info"`
	TitleRequests        int               `json:"title_requests"`
	TitleUsage           Usage             `json:"title_usage"`
	Clients              []string          `json:"clients"`
	IdentitySources      []string          `json:"identity_sources"`
	GeneratedRequests    int               `json:"generated_requests"`
	LinkedRequests       int               `json:"linked_requests"`
	Performance          Performance       `json:"performance"`
	TokenDetails         TokenDetails      `json:"token_details"`
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Archived             bool              `json:"archived"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
	PreferredAccounts    map[string]string `json:"preferred_accounts"`
	Requests             int               `json:"requests"`
	InFlight             int               `json:"in_flight"`
	Failures             int               `json:"failures"`
	UnknownUsageRequests int               `json:"requests_with_unknown_usage"`
	UnknownAttempts      int               `json:"attempts_without_usage"`
	Usage                Usage             `json:"usage"`
	Models               []string          `json:"models"`
}

type sessionMeta struct {
	NameInfo          NameInfo          `json:"name_info"`
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Archived          bool              `json:"archived"`
	CreatedAt         time.Time         `json:"created_at"`
	PreferredAccounts map[string]string `json:"preferred_accounts"`
}

type Result struct {
	PoolWaitMS     int64
	PoolWaits      int
	GeneratedTitle string
	TitleError     string
	TurnStateHash  string
	ToolCallIDs    []string
	Status         int
	Outcome        string
	Failure        Failure
	Response       Body
	ResponseID     string
	// Provider is the platform that actually served the request (the account
	// chosen by model-global routing); it overwrites the empty request-level
	// provider recorded at Begin when routing crosses platforms.
	Provider string
}

type Title struct {
	Detector        string   `json:"detector"`
	Association     string   `json:"association"`
	Status          string   `json:"status"`
	SourceMessages  []string `json:"source_message_hashes,omitempty"`
	LinkedRequestID string   `json:"linked_request_id,omitempty"`
	Text            string   `json:"text,omitempty"`
	Error           string   `json:"error,omitempty"`
}

type NameInfo struct {
	Source           string    `json:"source"`
	RequestID        string    `json:"request_id,omitempty"`
	Detector         string    `json:"detector,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	RequestStartedAt time.Time `json:"request_started_at,omitempty"`
}

// Identity describes evidence actually present on the wire. Client is an
// attribution hint, never an identity or an authentication boundary.
type Identity struct {
	Source          string `json:"source"`
	Client          string `json:"client"`
	ClientSessionID string `json:"client_session_id,omitempty"`
	LinkedRequestID string `json:"linked_request_id,omitempty"`
	Note            string `json:"note,omitempty"`
}
