package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"runtime"
	"strings"
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

// SetServerURL points bot_token fetch at octo-server.
func (c *Client) SetServerURL(serverURL string) {
	c.serverURL = strings.TrimRight(serverURL, "/")
}

// SetMatterURL points bot_task pull/ack and timeline writeback at
// octo-matter. PR-B: daemon talks to matter directly for tasks.
func (c *Client) SetMatterURL(matterURL string) {
	c.matterURL = strings.TrimRight(matterURL, "/")
}

// GetBotToken fetches the bot_token for the given bot_uid from
// octo-server. Requires SetServerURL + a working api_key.
func (c *Client) GetBotToken(ctx context.Context, botUID string) (string, error) {
	if c.serverURL == "" {
		return "", fmt.Errorf("server URL not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.serverURL+"/v1/bot/"+botUID+"/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	// RuntimeKind selects the runtime adapter for this task; see PendingBotTask.
	RuntimeKind string `json:"runtime_kind"`
}

// ErrMatterBatchUnsupported is returned by ListMatterBotTasksBatch when
// matter is older than the bot_uids batch endpoint and rejects the call
// with 400. Callers should fall back to per-bot ListMatterBotTasks.
var ErrMatterBatchUnsupported = errors.New("matter ListBotTasksBatch unsupported (server too old)")

// ListMatterBotTasks pulls up to limit queued tasks for the given bot_uid
// from matter (api_key auth via Bearer). Returns empty slice when nothing's
// queued. Retained as a fallback for pre-batch matter deployments.
func (c *Client) ListMatterBotTasks(ctx context.Context, botUID string, limit int) ([]MatterBotTask, error) {
	if c.matterURL == "" {
		return nil, fmt.Errorf("matter URL not set")
	}
	q := neturl.Values{}
	q.Set("bot_uid", botUID)
	q.Set("limit", fmt.Sprintf("%d", limit))
	url := fmt.Sprintf("%s/api/v1/internal/bot-tasks?%s", c.matterURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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

// ListMatterBotTasksBatch issues one HTTP call that claims up to perBotLimit
// queued tasks per bot_uid from matter (api_key auth via Bearer). The matter
// side runs the per-bot claims inside a single DB transaction so all-or-nothing
// at the batch level; per-bot limit preserves fairness (a noisy bot won't
// drown out quiet ones the way a flat limit would).
//
// Returned slice is flat — each MatterBotTask carries its own BotUID so
// the caller can group client-side without an extra round trip.
//
// Returns ErrMatterBatchUnsupported on HTTP 400 — matter is older than
// the bot_uids endpoint. Caller should fall back to per-bot pulls.
func (c *Client) ListMatterBotTasksBatch(ctx context.Context, botUIDs []string, perBotLimit int) ([]MatterBotTask, error) {
	if len(botUIDs) == 0 {
		return nil, nil
	}
	if c.matterURL == "" {
		return nil, fmt.Errorf("matter URL not set")
	}
	q := neturl.Values{}
	q.Set("bot_uids", strings.Join(botUIDs, ","))
	q.Set("limit", fmt.Sprintf("%d", perBotLimit))
	url := fmt.Sprintf("%s/api/v1/internal/bot-tasks?%s", c.matterURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("matter ListBotTasksBatch: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusBadRequest {
		return nil, ErrMatterBatchUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("matter ListBotTasksBatch %d: %s", resp.StatusCode, string(body))
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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

// WriteMatterTimeline posts a bot reply to matter timeline using the
// daemon's api_key Bearer (合并 plan 决策一+二 Phase 4 — DualAuth + AU5
// 已删, matter 端 AuthMiddleware + RequireKind(apikey) 验权).
func (c *Client) WriteMatterTimeline(ctx context.Context, matterID, actorUID, spaceID, content string) error {
	return c.matterInternalPost(ctx, fmt.Sprintf("/api/v1/internal/matters/%s/timeline", matterID),
		map[string]any{
			"actor_uid": actorUID,
			"space_id":  spaceID,
			"content":   content,
		})
}

// WriteMatterActivity posts an agent_task_* activity to matter via the
// api_key Bearer (合并 plan 决策一+二 Phase 4 — 同 WriteMatterTimeline).
func (c *Client) WriteMatterActivity(ctx context.Context, matterID, actorUID, action string, detail map[string]any, spaceID string) error {
	return c.matterInternalPost(ctx, fmt.Sprintf("/api/v1/internal/matters/%s/activities", matterID),
		map[string]any{
			"actor_uid": actorUID,
			"action":    action,
			"detail":    detail,
			"space_id":  spaceID,
		})
}

func (c *Client) matterInternalPost(ctx context.Context, path string, payload map[string]any) error {
	if c.matterURL == "" {
		return fmt.Errorf("matter URL not set")
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	RuntimeKind string `json:"runtime_kind"`
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
	// RuntimeKind selects which runtime adapter executes this task
	// (openclaw|claude|codex|hermes). Empty on pre-runtime-kind fleet builds,
	// where Registry.Get normalizes it to openclaw for backward compatibility.
	RuntimeKind string `json:"runtime_kind"`
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
