package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ForbiddenError struct {
	Message string
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("forbidden: %s", e.Message)
}

type Client struct {
	baseURL    string
	apiKey     string
	cliVersion string
	httpClient *http.Client

	serverURL string
	matterURL string
	useJWT    bool
	daemonID  string

	jwtMu     sync.Mutex
	jwtToken  string
	jwtExpiry time.Time
}

func NewClient(apiURL, apiKey, cliVersion string) *Client {
	baseURL := strings.TrimRight(apiURL, "/")
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		cliVersion: cliVersion,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetServerURL points the JWT exchange + bot_token fetch at octo-server.
// Once set, EnsureJWT and GetBotToken work.
func (c *Client) SetServerURL(serverURL string) {
	c.serverURL = strings.TrimRight(serverURL, "/")
}

// SetMatterURL points bot_task pull/ack and timeline writeback at
// octo-matter. PR-B: daemon talks to matter directly for tasks.
func (c *Client) SetMatterURL(matterURL string) {
	c.matterURL = strings.TrimRight(matterURL, "/")
}

// EnableJWT switches postJSON to use Bearer <JWT> instead of Bearer
// <api_key>. Must be called after SetServerURL so the client can
// actually fetch a JWT. daemonID is embedded into the JWT scope.
func (c *Client) EnableJWT(daemonID string) {
	c.useJWT = true
	c.daemonID = daemonID
}

// GetBotToken fetches the bot_token for the given bot_uid from
// octo-server. Requires SetServerURL + a working api_key.
func (c *Client) GetBotToken(ctx context.Context, botUID string) (string, error) {
	if c.serverURL == "" {
		return "", fmt.Errorf("server URL not set")
	}
	jwtTok, err := c.EnsureJWT(ctx, c.daemonID)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.serverURL+"/v1/bot/"+botUID+"/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GetBotToken %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		BotToken string `json:"bot_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	return r.BotToken, nil
}

// --- PR-B matter client methods ---

// MatterBotTask is the daemon-side shape of GET /api/v1/internal/bot-tasks.
type MatterBotTask struct {
	ID          string `json:"id"`
	MatterID    string `json:"matter_id"`
	SpaceID     string `json:"space_id"`
	BotUID      string `json:"bot_uid"`
	Prompt      string `json:"prompt"`
	MatterTitle string `json:"matter_title,omitempty"`
	ClaimToken  string `json:"claim_token"`
	LeaseUntil  string `json:"lease_until"`
}

// ListMatterBotTasks pulls up to limit queued tasks for the given bot_uid
// from matter (daemon JWT auth). Returns empty slice when nothing's queued.
func (c *Client) ListMatterBotTasks(ctx context.Context, botUID string, limit int) ([]MatterBotTask, error) {
	if c.matterURL == "" {
		return nil, fmt.Errorf("matter URL not set")
	}
	jwtTok, err := c.EnsureJWT(ctx, c.daemonID)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/api/v1/internal/bot-tasks?bot_uid=%s&limit=%d",
		c.matterURL, botUID, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("matter ListBotTasks: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("matter ListBotTasks %d: %s", resp.StatusCode, string(body))
	}
	var r struct {
		Tasks []MatterBotTask `json:"tasks"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Tasks, nil
}

// AckMatterBotTask reports the outcome of a claimed task back to matter.
// Status must be "succeeded" or "failed". 409 = claim_token mismatch
// (daemon should drop result, not retry).
func (c *Client) AckMatterBotTask(ctx context.Context, taskID, claimToken, status, errMsg, resultSummary string, elapsedMs int64) error {
	if c.matterURL == "" {
		return fmt.Errorf("matter URL not set")
	}
	jwtTok, err := c.EnsureJWT(ctx, c.daemonID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"claim_token":    claimToken,
		"status":         status,
		"error_msg":      errMsg,
		"result_summary": resultSummary,
		"elapsed_ms":     elapsedMs,
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/v1/internal/bot-tasks/%s/ack", c.matterURL, taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("matter ack: %w", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("ack 409 (claim_token stale, drop result): %s", string(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("matter ack %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// WriteMatterTimeline posts a bot reply to matter timeline. Uses the
// daemon's own JWT — matter's writeback endpoints accept either daemon
// JWT or X-Internal-Token (DualAuth); we always use JWT so the daemon
// never needs the shared NOTIFY_INTERNAL_TOKEN secret on the user's
// machine.
//
// taskID + claimToken bind this writeback to the in-flight bot_task
// daemon just claimed; matter cross-checks them against matter_bot_task
// to enforce "you can only write under bots whose tasks you currently
// hold" (closes the actor_uid spoofing gap on the JWT path).
func (c *Client) WriteMatterTimeline(ctx context.Context, matterID, actorUID, spaceID, content, taskID, claimToken string) error {
	return c.matterInternalPost(ctx, fmt.Sprintf("/api/v1/internal/matters/%s/timeline", matterID),
		map[string]any{
			"actor_uid":   actorUID,
			"space_id":    spaceID,
			"content":     content,
			"task_id":     taskID,
			"claim_token": claimToken,
		})
}

// WriteMatterActivity posts an agent_task_* activity to matter via the
// daemon JWT path (same DualAuth endpoint as timeline). taskID + claimToken
// serve the same writeback-context binding role as in WriteMatterTimeline.
func (c *Client) WriteMatterActivity(ctx context.Context, matterID, actorUID, action string, detail map[string]any, spaceID, taskID, claimToken string) error {
	return c.matterInternalPost(ctx, fmt.Sprintf("/api/v1/internal/matters/%s/activities", matterID),
		map[string]any{
			"actor_uid":   actorUID,
			"action":      action,
			"detail":      detail,
			"space_id":    spaceID,
			"task_id":     taskID,
			"claim_token": claimToken,
		})
}

func (c *Client) matterInternalPost(ctx context.Context, path string, payload map[string]any) error {
	if c.matterURL == "" {
		return fmt.Errorf("matter URL not set")
	}
	jwtTok, err := c.EnsureJWT(ctx, c.daemonID)
	if err != nil {
		return fmt.Errorf("ensure jwt: %w", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.matterURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("matter post: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("matter %s %d: %s", path, resp.StatusCode, string(rb))
	}
	return nil
}

type tokenExchangeReq struct {
	APIKey   string `json:"api_key"`
	DaemonID string `json:"daemon_id,omitempty"`
}

type tokenExchangeResp struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
	Scope     string `json:"scope"`
}

// EnsureJWT fetches (or refreshes if within 5min of expiry) a daemon-scope
// JWT from octo-server using the configured api-key. Safe for concurrent
// callers via jwtMu. Returns the bearer token string.
func (c *Client) EnsureJWT(ctx context.Context, daemonID string) (string, error) {
	if c.serverURL == "" {
		return "", fmt.Errorf("server URL not set — call SetServerURL first")
	}
	c.jwtMu.Lock()
	defer c.jwtMu.Unlock()
	if c.jwtToken != "" && time.Until(c.jwtExpiry) > 5*time.Minute {
		return c.jwtToken, nil
	}
	body, err := json.Marshal(tokenExchangeReq{APIKey: c.apiKey, DaemonID: daemonID})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.serverURL+"/v1/auth/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("token exchange %d: %s", resp.StatusCode, string(respBody))
	}
	var r tokenExchangeResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	c.jwtToken = r.Token
	c.jwtExpiry = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	return r.Token, nil
}

type RegisterRequest struct {
	DaemonID            string        `json:"daemon_id"`
	DeviceName          string        `json:"device_name"`
	DeviceInfo          string        `json:"device_info"`
	CLIVersion          string        `json:"cli_version"`
	HeartbeatIntervalMs int64         `json:"heartbeat_interval_ms,omitempty"` // fleet uses for per-runtime stale = 3× this
	Runtimes            []RuntimeInfo `json:"runtimes"`
}

type RegisteredRuntime struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
}

type RegisterResponse struct {
	Runtimes []RegisteredRuntime `json:"runtimes"`
}

func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	if err := c.postJSON(ctx, "/v1/daemon/register", req, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	return &resp, nil
}

type HeartbeatRequest struct {
	RuntimeID int64 `json:"runtime_id"`
}

type PendingPing struct {
	PingID string `json:"ping_id"`
}

type PendingUpgrade struct {
	TaskID        string `json:"task_id"`
	Component     string `json:"component"` // "octo-daemon" or "octo"
	DownloadURL   string `json:"download_url"`
	TargetVersion string `json:"target_version"`
	Checksum      string `json:"checksum"`
	Metadata      string `json:"metadata"` // 预留字段，插件升级未使用
}

// PendingAgentCommand is the heartbeat-pull dispatch envelope.
// PoC4 introduces a new action "bot.provision" that combines workspace
// creation + bot binding in one daemon step; the old "agent.create" and
// "bot.add" actions are removed.
type PendingAgentCommand struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"` // "bot.provision"
	WorkspaceID string `json:"workspace_id"`
	DisplayName string `json:"display_name"`
	BotUID      string `json:"bot_uid"`
	BotToken    string `json:"bot_token"`
	APIURL      string `json:"api_url"`
	ClaimToken  string `json:"claim_token"`
}

type HeartbeatResponse struct {
	Status         string               `json:"status"`
	PendingPing    *PendingPing         `json:"pending_ping,omitempty"`
	PendingUpgrade *PendingUpgrade      `json:"pending_upgrade,omitempty"`
	PendingCommand *PendingAgentCommand `json:"pending_command,omitempty"`
	PendingTask    *PendingBotTask      `json:"pending_task,omitempty"`
	// PR-B.2: fleet returns the daemon's managed bot list so daemon can
	// pull tasks for each from matter directly. Empty / missing on
	// pre-PR-B.2 fleet builds — daemon falls back to PendingTask.
	ManagedBots []ManagedBot `json:"managed_bots,omitempty"`
}

// ManagedBot identifies a bot whose runtime is hosted by this daemon.
// Daemon iterates these on each heartbeat tick to pull bot_tasks from
// matter.
type ManagedBot struct {
	BotUID      string `json:"bot_uid"`
	WorkspaceID string `json:"workspace_id"`
}

// PendingBotTask is the server's pull-mode dispatch for a matter-driven
// agent task. Server has already resolved bot_uid → agent_id binding; we
// just spawn `openclaw agent --agent <id>` with the prompt and ack back.
type PendingBotTask struct {
	ID         int64  `json:"id"`
	AgentID    string `json:"agent_id"`
	Prompt     string `json:"prompt"`
	MatterID   string `json:"matter_id"`
	BotUID     string `json:"bot_uid"`
	ClaimToken string `json:"claim_token"`
}

func (c *Client) Heartbeat(ctx context.Context, runtimeID int64) (*HeartbeatResponse, error) {
	req := HeartbeatRequest{RuntimeID: runtimeID}
	var resp HeartbeatResponse
	if err := c.postJSON(ctx, "/v1/daemon/heartbeat", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ReportPing(ctx context.Context, pingID string) error {
	return c.postJSON(ctx, "/v1/daemon/ping/"+pingID, map[string]string{}, nil)
}

func (c *Client) ReportUpgrade(ctx context.Context, taskID, status, errMsg string) error {
	return c.postJSON(ctx, "/v1/daemon/upgrade/"+taskID, map[string]string{
		"status": status, "error": errMsg,
	}, nil)
}

// AckBot acknowledges completion (or failure) of a bot.provision command.
// Path mirrors server's daemon route group.
func (c *Client) AckBot(ctx context.Context, id int64, claimToken, status, errMsg string) error {
	path := fmt.Sprintf("/v1/daemon/bots/%d/ack", id)
	return c.postJSON(ctx, path, map[string]string{
		"claim_token": claimToken,
		"status":      status,
		"error_msg":   errMsg,
	}, nil)
}

// AckBotTask reports the outcome of a matter-driven agent task. status must
// be "succeeded" or "failed"; on success, result_summary holds the agent
// reply (truncated for storage); on failure, error_msg explains why.
func (c *Client) AckBotTask(ctx context.Context, id int64, claimToken, status, resultSummary, errMsg string) error {
	path := fmt.Sprintf("/v1/daemon/bot-tasks/%d/ack", id)
	return c.postJSON(ctx, path, map[string]string{
		"claim_token":    claimToken,
		"status":         status,
		"result_summary": resultSummary,
		"error_msg":      errMsg,
	}, nil)
}

type DeregisterRequest struct {
	RuntimeIDs []int64 `json:"runtime_ids"`
}

func (c *Client) Deregister(ctx context.Context, runtimeIDs []int64) error {
	req := DeregisterRequest{RuntimeIDs: runtimeIDs}
	return c.postJSON(ctx, "/v1/daemon/deregister", req, nil)
}

func (c *Client) postJSON(ctx context.Context, path string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	authToken := c.apiKey
	if c.useJWT {
		jwtTok, jerr := c.EnsureJWT(ctx, c.daemonID)
		if jerr != nil {
			return fmt.Errorf("ensure jwt: %w", jerr)
		}
		authToken = jwtTok
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("X-Client-Platform", "daemon")
	req.Header.Set("X-Client-Version", c.cliVersion)
	req.Header.Set("X-Client-OS", normalizeOS())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == 403 {
		return &ForbiddenError{Message: string(respBody)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}

	return nil
}

func normalizeOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "linux":
		return "linux"
	case "windows":
		return "windows"
	default:
		return runtime.GOOS
	}
}
