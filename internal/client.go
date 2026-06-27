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

// maxResponseSize caps how much of an HTTP response body the daemon reads into
// memory, so a misbehaving or compromised upstream cannot OOM the daemon. All
// responses here are small JSON; 1 MiB is generous.
const maxResponseSize = 1 << 20

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("forbidden: %s", e.Message)
}

// ErrCcOctoConfigUnavailable signals that the cc-octo install secret cannot be
// fetched for a non-retryable reason: the task is terminal/stale, the in-flight
// secret is missing/expired, or an ownership/validation rejection (any 4xx).
// fleet returns CONFLICT/NOT_FOUND/FORBIDDEN here (no dedicated wire code — the
// error-code set is fixed at 12). The daemon skips such an event idempotently
// (no report) to avoid a report-failed→rejected-transition→replay loop; a
// genuinely missing in-flight secret is reclaimed by fleet's sweeper timeout.
// Transient (5xx / network) errors are returned as plain errors instead, so the
// SSE/heartbeat path retries them.
var ErrCcOctoConfigUnavailable = errors.New("cc-octo install config unavailable")

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

// CcOctoConfig holds the cc-octo install secret: LLM gateway URL + API key.
type CcOctoConfig struct {
	GatewayURL string `json:"gateway_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
}

// FetchCcOctoConfig pulls the cc-octo install secret (LLM gateway url + key)
// for an upgrade task out of band — the secret never rides the SSE/upgrade
// payload (same pattern as bot.provision fetch).
//
//	Status mapping:
//	  200 (well-formed)        → (*CcOctoConfig, nil)
//	  200 (empty/bad payload)  → (nil, plain transient error): retry — a one-off
//	      bad/empty body shouldn't make daemon permanently abandon the install
//	  404 → (nil, nil): no install secret for this task (plain upgrade path)
//	  any other 4xx (409 terminal/in-flight-missing, 403, 400)
//	      → (nil, ErrCcOctoConfigUnavailable wrapped): skip idempotently
//	  5xx / transport error → (nil, plain transient error): retry
func (c *Client) FetchCcOctoConfig(ctx context.Context, runtimeID int64, taskID string) (*CcOctoConfig, error) {
	url := fmt.Sprintf("%s/v1/upgrades/%s/cc-octo-config?runtime_id=%d", c.baseURL, taskID, runtimeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))

	switch {
	case resp.StatusCode == http.StatusOK:
		// fall through to unmarshal below
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil // no install secret for this task → plain upgrade path
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// 409 (terminal/stale or in-flight-missing), 403, 400, etc. — non-retryable;
		// skip the event idempotently rather than report failed (avoids a
		// report-failed→rejected-transition→replay loop on terminal tasks).
		return nil, fmt.Errorf("fetch cc-octo config: status %d: %s: %w", resp.StatusCode, string(body), ErrCcOctoConfigUnavailable)
	default:
		// 5xx or transport-level errors remain plain transient errors → retry
		return nil, fmt.Errorf("fetch cc-octo config: status %d: %s", resp.StatusCode, string(body))
	}

	var env struct {
		Data CcOctoConfig `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("fetch cc-octo config unmarshal: %w", err)
	}
	if env.Data.GatewayURL == "" || env.Data.APIKey == "" {
		// 200 但 payload 空/字段缺失 → 当作服务端瞬时协议错误(可重试),不归入
		// ErrCcOctoConfigUnavailable —— 后者是 4xx 终态语义,会让 daemon 永久跳过该安装;
		// 一次坏/空的 200 不该让整个安装放弃,交给 SSE/心跳重试。
		return nil, fmt.Errorf("fetch cc-octo config: empty payload from a 200 response")
	}
	return &env.Data, nil
}

type RegisterRequest struct {
	DaemonID            string        `json:"daemon_id"`
	DeviceName          string        `json:"device_name"`
	DeviceInfo          string        `json:"device_info"` // JSON: {os, arch, os_version, device_id}
	CLIVersion          string        `json:"cli_version"`
	HeartbeatIntervalMs int64         `json:"heartbeat_interval_ms,omitempty"` // fleet uses for per-runtime stale = 3× this
	Runtimes            []RuntimeInfo `json:"runtimes"`
	// No omitempty: an empty slice is a meaningful "full inventory is empty"
	// signal (server replaces the reported set), distinct from "no update".
	// parseDeviceComponents returns a non-nil empty slice, so this serializes as
	// [] rather than null.
	DeviceComponents []DeviceComponent `json:"device_components"`
}

// DeviceComponent is a machine-level (not runtime-level) software component
// reported in the register payload. It backs machine_component.reported_version
// in the desired/reported reconciliation model. Currently sourced from npm
// global packages; Type leaves room for other distribution formats later.
type DeviceComponent struct {
	Type         string `json:"type"`          // package-manager/runtime type; currently only "nodejs"
	Name         string `json:"name"`          // short name without scope, e.g. "cc-channel-octo"
	ComponentKey string `json:"component_key"` // full npm package name, e.g. "@mininglamp-oss/cc-channel-octo"
	Version      string `json:"version"`       // installed version; "" when not installed
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
	if err := c.postJSON(ctx, "/v1/runtimes", req, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	return &resp, nil
}

// VerifyResponse echoes the owner + space resolved from the api_key. fleet
// performs no DB write — the auth middleware already validated the key against
// octo-server; this lets `config` confirm the key and learn its bound space_id.
type VerifyResponse struct {
	SpaceID  string `json:"space_id"`
	OwnerUID string `json:"owner_uid"`
}

// Verify validates the api_key against fleet and returns the bound space.
// Used by `config` as a setup gate before any profile is persisted.
func (c *Client) Verify(ctx context.Context) (*VerifyResponse, error) {
	var resp VerifyResponse
	if err := c.postJSON(ctx, "/v1/runtimes/verify", struct{}{}, &resp); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	return &resp, nil
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
	PendingUpgrade *PendingUpgrade      `json:"pending_upgrade,omitempty"`
	PendingCommand *PendingAgentCommand `json:"pending_command,omitempty"`
	// PR-B.2: fleet returns the daemon's managed bot list so daemon can
	// pull tasks for each from matter directly.
	ManagedBots []ManagedBot `json:"managed_bots,omitempty"`
}

// ManagedBot identifies a bot whose runtime is hosted by this daemon.
// Daemon iterates these on each heartbeat tick to pull bot_tasks from
// matter.
type ManagedBot struct {
	BotUID      string `json:"bot_uid"`
	WorkspaceID string `json:"workspace_id"`
}

func (c *Client) Heartbeat(ctx context.Context, runtimeID int64) (*HeartbeatResponse, error) {
	var resp HeartbeatResponse
	path := fmt.Sprintf("/v1/runtimes/%d/heartbeat", runtimeID)
	if err := c.postJSON(ctx, path, struct{}{}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DaemonHeartbeatRequest carries the payload for a daemon-level liveness heartbeat.
type DaemonHeartbeatRequest struct {
	DaemonID          string `json:"daemon_id"`
	DeviceUUID        string `json:"device_uuid"`
	HeartbeatIntervalMs int64 `json:"heartbeat_interval_ms"`
}

// DaemonHeartbeat sends a daemon-level liveness heartbeat to maintain the
// device-level online indicator ("green dot"). best-effort: caller should log
// failures without interrupting the main loop.
func (c *Client) DaemonHeartbeat(ctx context.Context, req DaemonHeartbeatRequest) error {
	return c.postJSON(ctx, "/v1/daemons/heartbeat", req, nil)
}

func (c *Client) ReportUpgrade(ctx context.Context, taskID, status, errMsg string) error {
	return c.postJSON(ctx, "/v1/upgrades/"+taskID+"/report", map[string]string{
		"status": status, "error": errMsg,
	}, nil)
}

// AckBot acknowledges completion (or failure) of a bot.provision command.
func (c *Client) AckBot(ctx context.Context, id int64, claimToken, status, errMsg string) error {
	path := fmt.Sprintf("/v1/bots/%d/ack", id)
	return c.postJSON(ctx, path, map[string]string{
		"claim_token": claimToken,
		"status":      status,
		"error_msg":   errMsg,
	}, nil)
}

type DeregisterRequest struct {
	RuntimeIDs []int64 `json:"runtime_ids"`
}

func (c *Client) Deregister(ctx context.Context, runtimeIDs []int64) error {
	req := DeregisterRequest{RuntimeIDs: runtimeIDs}
	return c.postJSON(ctx, "/v1/runtimes/_deregister", req, nil)
}

// providersResp 对齐 fleet GET /v1/providers 的 data payload。
type providersResp struct {
	Providers []fleetProvider `json:"providers"`
}

type listProvidersResp struct {
	Data providersResp `json:"data"`
}

// ListProviders 拉取 fleet 当前启用的 runtime provider 列表 (#52)。403 →
// ForbiddenError 让 daemon checkForbidden 退出,与 postJSON 语义一致。
func (c *Client) ListProviders(ctx context.Context) ([]fleetProvider, error) {
	url := c.baseURL + "/v1/providers"
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
		return nil, &ForbiddenError{Message: string(body)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list providers status %d: %s", resp.StatusCode, string(body))
	}
	var out listProvidersResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("unmarshal providers: %w", err)
	}
	return out.Data.Providers, nil
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
		// fleet renders errors as {"error":{"code","message",...}} (R2).
		var env struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &env) == nil && env.Error != nil {
			return fmt.Errorf("fleet %d %s: %s", resp.StatusCode, env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		// fleet wraps success payloads in a {"data": ...} envelope (R1).
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(respBody, &env); err != nil {
			return fmt.Errorf("unmarshal envelope: %w", err)
		}
		if len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, result); err != nil {
				return fmt.Errorf("unmarshal data: %w", err)
			}
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
