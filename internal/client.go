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

type RegisterRequest struct {
	DaemonID   string        `json:"daemon_id"`
	DeviceName string        `json:"device_name"`
	DeviceInfo string        `json:"device_info"`
	CLIVersion string        `json:"cli_version"`
	Runtimes   []RuntimeInfo `json:"runtimes"`
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

// PendingAgentCommand is the server's pull-mode dispatch for a managed agent
// provisioning task. Returned in heartbeat response when there's work to do.
// The daemon must run the side-effects and then POST back via AckManagedAgent
// with the matching ClaimToken.
type PendingAgentCommand struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"` // "agent.create" for now
	AgentID     string `json:"agent_id"`
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

// AckManagedAgent reports the result of an agent.create command back to the
// server. The claim_token must match what the server set when dispatching, or
// the server will reject as stale (HTTP 409).
func (c *Client) AckManagedAgent(ctx context.Context, id int64, claimToken, status, errMsg string) error {
	path := fmt.Sprintf("/v1/daemon/managed-agents/%d/ack", id)
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
