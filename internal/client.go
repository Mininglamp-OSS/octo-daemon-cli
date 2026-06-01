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

	// PR-A.2: fleet/server split.
	//   baseURL  — fleet base (default :8092). All /v1/daemon and
	//              /v1/runtimes routes live here now.
	//   serverURL — server base (default :8090). Only used for
	//              /v1/auth/token (api_key → JWT) and
	//              /v1/bot/:uid/token (daemon JWT → bot_token).
	// useJWT — when true, postJSON sends Authorization: Bearer <JWT>
	//          (auto-refreshed via EnsureJWT). When false, sends the
	//          old Authorization: Bearer <api_key>. Tests / staged
	//          rollouts can flip per Client.
	serverURL string
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
