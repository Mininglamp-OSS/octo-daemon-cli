package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	goruntime "runtime"
	"regexp"
	"strings"
	"sync"
	"time"
)

var versionRe = regexp.MustCompile(`v?(\d+\.\d+\.\d+)`)

type RuntimeInfo struct {
	Provider string       `json:"type"`
	Name     string       `json:"name"`
	Version  string       `json:"version"`
	Status   string       `json:"status"`
	Path     string       `json:"-"`
	Agents   []AgentEntry `json:"agents,omitempty"`
	Plugins  []PluginInfo `json:"plugins,omitempty"`
}

type AgentEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Bindings int      `json:"bindings"`
	Default  bool     `json:"is_default"`
	Routes   []string `json:"routes,omitempty"`
}

func GetDeviceInfo() string {
	info := map[string]string{
		"os":   goruntime.GOOS,
		"arch": goruntime.GOARCH,
	}
	data, _ := json.Marshal(info)
	return string(data)
}

var providers = map[string]string{
	"claude":   "claude",
	"codex":    "codex",
	"openclaw": "openclaw",
	"hermes":   "hermes",
}

// DetectRuntimesFast does quick detection only (LookPath + version + gateway port probe).
// Returns immediately without waiting for slow operations like `openclaw agents list`.
func DetectRuntimesFast() []RuntimeInfo {
	type result struct {
		rt    RuntimeInfo
		found bool
	}

	ch := make(chan result, len(providers))

	for provider, binary := range providers {
		go func(provider, binary string) {
			binPath, err := exec.LookPath(binary)
			if err != nil {
				ch <- result{found: false}
				return
			}
			version := detectVersion(binPath)
			status := "online"
			switch provider {
			case "openclaw":
				gwRunning := isOpenclawGatewayRunning(binPath)
				log.Printf("[DEBUG] openclaw gateway running: %v", gwRunning)
				if !gwRunning {
					status = "offline"
				}
			case "claude":
				// claude bot 实际由 cc-channel-octo gateway 接 IM WS 派发到
				// Claude SDK; cc-channel-octo 进程死了 → claude bot 无法响应
				// 消息. 这里探的不是 claude CLI binary (LookPath 已确认装了),
				// 而是 cc-channel-octo gateway 进程死活. 跟 isOpenclawGatewayRunning
				// 同模式: fork 子命令解析人类格式输出, 等 cc-channel-octo
				// 暴露 --json 契约面后再迁结构化解析.
				gwRunning := isCcChannelOctoRunning()
				log.Printf("[DEBUG] cc-channel-octo gateway running: %v", gwRunning)
				if !gwRunning {
					status = "offline"
				}
			}
			rt := RuntimeInfo{
				Provider: provider,
				Name:     provider,
				Version:  version,
				Status:   status,
				Path:     binPath,
			}
			// Plugins detection is moved to the slow enrich path (EnrichOpenclawRuntime),
			// because `openclaw plugins list --json` needs openclaw init (seconds).
			ch <- result{rt: rt, found: true}
		}(provider, binary)
	}

	var runtimes []RuntimeInfo
	for range providers {
		r := <-ch
		if r.found {
			runtimes = append(runtimes, r.rt)
		}
	}
	return runtimes
}

// EnrichOpenclawRuntime runs the slow `openclaw agents list --json`,
// `openclaw agents bindings --json`, and `openclaw plugins list --json` in
// parallel for each openclaw runtime. Failures on any probe are isolated —
// the other fields still get populated. Call this asynchronously after initial
// fast registration.
func EnrichOpenclawRuntime(runtimes []RuntimeInfo) []RuntimeInfo {
	enriched := make([]RuntimeInfo, len(runtimes))
	copy(enriched, runtimes)
	for i := range enriched {
		if enriched[i].Provider != "openclaw" || enriched[i].Path == "" {
			continue
		}
		binPath := enriched[i].Path

		var agents []AgentEntry
		var plugins []PluginInfo
		var bindings map[string][]string
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			agents = DetectOpenclawAgents(binPath)
		}()
		go func() {
			defer wg.Done()
			plugins = DetectOpenclawPlugins(binPath)
		}()
		go func() {
			defer wg.Done()
			bindings = DetectOpenclawBindings(binPath)
		}()
		wg.Wait()

		// Merge bindings into agent.Routes. openclaw 2026.5.4+ moved routes
		// out of `agents list` into a separate `agents bindings` command, so
		// this is now the only path that populates routes.
		if len(bindings) > 0 && len(agents) > 0 {
			mergeBindingsIntoAgents(agents, bindings)
		}

		if len(agents) > 0 {
			enriched[i].Agents = agents
			log.Printf("[INFO]   └─ %d agent(s): %s", len(agents), agentIDs(agents))
		}
		if len(plugins) > 0 {
			enriched[i].Plugins = plugins
			log.Printf("[INFO]   └─ %d plugin(s): %s", len(plugins), pluginNames(plugins))
		}
	}
	return enriched
}

// EnrichOpenclawAgents is kept as a thin alias for callers that haven't migrated
// to EnrichOpenclawRuntime. New code should use EnrichOpenclawRuntime directly.
func EnrichOpenclawAgents(runtimes []RuntimeInfo) []RuntimeInfo {
	return EnrichOpenclawRuntime(runtimes)
}

func pluginNames(plugins []PluginInfo) string {
	names := make([]string, len(plugins))
	for i, p := range plugins {
		names[i] = fmt.Sprintf("%s@%s", p.Name, p.Version)
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// DetectRuntimes does full detection including slow operations (backward compat).
func DetectRuntimes() []RuntimeInfo {
	runtimes := DetectRuntimesFast()
	return EnrichOpenclawRuntime(runtimes)
}


type openclawAgentJSON struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Bindings  int      `json:"bindings"`
	IsDefault bool     `json:"isDefault"`
	Routes    []string `json:"routes"`
}

func DetectOpenclawAgents(binPath string) []AgentEntry {
	if binPath == "" {
		p, err := exec.LookPath("openclaw")
		if err != nil {
			return nil
		}
		binPath = p
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "agents", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	// Strip non-JSON prefix lines (e.g. "[dmwork] registering ...")
	// Find the line that is exactly "[" (the JSON array start)
	out = extractJSONArray(out)
	if out == nil {
		return nil
	}

	var raw []openclawAgentJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil
	}

	agents := make([]AgentEntry, 0, len(raw))
	for _, a := range raw {
		name := a.Name
		if name == "" {
			name = a.ID
		}
		agents = append(agents, AgentEntry{
			ID:       a.ID,
			Name:     name,
			Bindings: a.Bindings,
			Default:  a.IsDefault,
			Routes:   a.Routes,
		})
	}
	return agents
}

func detectVersion(binPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}

	raw := strings.TrimSpace(string(out))
	// 正则捕获组 [1] 拿到剥掉 "v" 前缀的纯数字版本。
	// 不用 FindString —— 那个会把 "v0.13.0" 整体返回，破坏服务端
	// (daemon_id, component, version) 关单匹配（target 侧都是无 v 的）。
	if m := versionRe.FindStringSubmatch(raw); len(m) > 1 && m[1] != "" {
		return m[1]
	}
	return raw
}

func capitalize(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// isOpenclawGatewayRunning parses `openclaw gateway status` output to determine
// if the gateway is actually running. It checks the "Probe target" URL and
// probes that port, respecting whatever IP/port the user has configured.
func isOpenclawGatewayRunning(binPath string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "gateway", "status")
	out, _ := cmd.CombinedOutput()
	if len(out) == 0 {
		return false
	}

	output := string(out)

	// Parse "Probe target: ws://host:port" to get the actual address
	for _, line := range strings.Split(output, "\n") {
		// Strip ANSI escape codes
		clean := stripAnsi(line)
		clean = strings.TrimSpace(clean)
		if strings.HasPrefix(clean, "Probe target:") {
			target := strings.TrimSpace(strings.TrimPrefix(clean, "Probe target:"))
			target = strings.TrimPrefix(target, "ws://")
			target = strings.TrimPrefix(target, "wss://")
			if target != "" {
				conn, dialErr := net.DialTimeout("tcp", target, 2*time.Second)
				if dialErr != nil {
					log.Printf("[DEBUG] openclaw probe %s failed: %v", target, dialErr)
					return false
				}
				conn.Close()
				return true
			}
		}
	}

	log.Printf("[DEBUG] openclaw gateway status: no 'Probe target' found in output")
	return false
}

// isCcChannelOctoRunning 通过 fork `cc-channel-octo status` 解析输出判断
// gateway 进程是否在跑. cc-channel-octo binary 不在 PATH 时直接返 false
// (= claude bot 跑不起来 = 标 offline, 跟"cc-channel-octo 死了"语义一致).
//
// status 命令输出有两种 (实测纯 ASCII, 无 ANSI 转义):
//   "cc-channel-octo: running (pid 1306), logs at /Users/.../gateway.log"
//   "cc-channel-octo: stopped"
// 用 ": running" 子串匹配区分死活. "not running" / "stopped" 都不含此子串.
//
// 跟 isOpenclawGatewayRunning 同模式 (fork 子命令 + 解析人类格式), 等
// cc-channel-octo 加 --json 契约面后再迁结构化解析.
func isCcChannelOctoRunning() bool {
	binPath, err := exec.LookPath("cc-channel-octo")
	if err != nil {
		log.Printf("[DEBUG] cc-channel-octo not in PATH: %v", err)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[DEBUG] cc-channel-octo status failed: %v", err)
		return false
	}
	return strings.Contains(stripAnsi(string(out)), ": running")
}

func stripAnsi(s string) string {
	const ansiEscape = '\033'
	var result []byte
	inEscape := false
	for i := 0; i < len(s); i++ {
		if s[i] == byte(ansiEscape) {
			inEscape = true
			continue
		}
		if inEscape {
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				inEscape = false
			}
			continue
		}
		result = append(result, s[i])
	}
	return string(result)
}

type PluginInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// extractJSONArray extracts a JSON array from output that may have non-JSON
// lines before and/or after it. Handles:
//   - Pretty-printed: lines starting with "[" and ending with "]"
//   - Single-line: [{"id":"main",...}]
//   - Mixed with log prefixes
func extractJSONArray(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)

	// Try 1: entire output is valid JSON array
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var test []json.RawMessage
		if json.Unmarshal(trimmed, &test) == nil {
			return trimmed
		}
	}

	// Try 2: find "[" ... "]" span by lines (pretty-printed with prefix/suffix noise)
	lines := bytes.Split(data, []byte("\n"))
	start := -1
	end := -1
	for i, line := range lines {
		t := bytes.TrimSpace(line)
		if start == -1 && bytes.Equal(t, []byte("[")) {
			start = i
		}
		if start != -1 && bytes.Equal(t, []byte("]")) {
			end = i
		}
	}
	if start >= 0 && end >= start {
		candidate := bytes.Join(lines[start:end+1], []byte("\n"))
		var test []json.RawMessage
		if json.Unmarshal(candidate, &test) == nil {
			return candidate
		}
	}

	// Try 3: scan for each "[" and try pairing with last "]" after it
	for i := 0; i < len(data); i++ {
		if data[i] != '[' {
			continue
		}
		lastBracket := bytes.LastIndexByte(data[i:], ']')
		if lastBracket <= 0 {
			continue
		}
		candidate := data[i : i+lastBracket+1]
		var test []json.RawMessage
		if json.Unmarshal(candidate, &test) == nil {
			return candidate
		}
	}

	return nil
}
