package internal

import (
	"bytes"
	"context"
	"encoding/json"
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

// maxResponseSize caps how much of an HTTP response body the daemon reads into
// memory, so a misbehaving or compromised upstream cannot OOM the daemon. All
// responses here are small JSON; 1 MiB is generous.
const maxResponseSize = 1 << 20

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("forbidden: %s", e.Message)
}

type Client struct {
	baseURL    string
	apiKey     string
	cliVersion string
	httpClient *http.Client

	serverURL string
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

// GetBotToken fetches the bot_token for the given bot_uid from
// octo-server. Requires SetServerURL + a working api_key.
func (c *Client) GetBotToken(ctx context.Context, botUID string) (string, error) {
	if c.serverURL == "" {
		return "", fmt.Errorf("server URL not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.serverURL+"/v1/bot/"+neturl.PathEscape(botUID)+"/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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
	// (openclaw|claude). Empty on pre-runtime-kind fleet builds,
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
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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

// listProvidersResp 对齐 fleet GET /v1/daemon/runtime-providers 响应。
type listProvidersResp struct {
	Providers []fleetProvider `json:"providers"`
}

// ListProviders 拉 fleet active provider 列表(PR-A 端点)。
// 老 fleet 无此端点会 404 → 返错,调用方保留上次快照 / 编译期 fallback。
func (c *Client) ListProviders(ctx context.Context) ([]fleetProvider, error) {
	url := c.baseURL + "/v1/daemon/runtime-providers"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-Client-Platform", "daemon")
	req.Header.Set("X-Client-Version", c.cliVersion)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if resp.StatusCode == http.StatusForbidden {
		// 与 postJSON 一致:403 → ForbiddenError,daemon checkForbidden 退出。
		return nil, &ForbiddenError{Message: string(body)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list providers status %d: %s", resp.StatusCode, string(body))
	}
	var out listProvidersResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("unmarshal providers: %w", err)
	}
	return out.Providers, nil
}
