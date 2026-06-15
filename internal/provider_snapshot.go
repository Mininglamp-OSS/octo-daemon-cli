package internal

import "sync/atomic"

// fleetProvider 是 GET /v1/daemon/runtime-providers 响应里的一行。
// 字段对齐 fleet providerInfo(api.go):name/display_name/binary_name/upgrade_timeout_sec。
type fleetProvider struct {
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	BinaryName        string `json:"binary_name"`
	UpgradeTimeoutSec int    `json:"upgrade_timeout_sec"`
}

// providerSnapshot 存不可变的 provider→binary map。用 atomic.Value 整体替换,
// 避免 detect / register / slow-detect 并发读写 Go map 触发
// "fatal error: concurrent map iteration and map write"(codex BLOCKER 3)。
var providerSnapshot atomic.Value // map[string]string

// fallbackProviders 是编译期兜底:fleet 不可达 / 老 fleet 无端点时使用。
// 本期只放行 claude + openclaw(去掉 codex/hermes)。
var fallbackProviders = map[string]string{
	"claude":   "claude",
	"openclaw": "openclaw",
}

func init() {
	resetProvidersForTest()
}

// resetProvidersForTest 把快照重置为编译期 fallback(init + 测试用)。
func resetProvidersForTest() {
	cp := make(map[string]string, len(fallbackProviders))
	for k, v := range fallbackProviders {
		cp[k] = v
	}
	providerSnapshot.Store(cp)
}

// currentProviders 返回当前快照的防御性拷贝(调用方改返回值不污染内部)。
func currentProviders() map[string]string {
	cur, _ := providerSnapshot.Load().(map[string]string)
	out := make(map[string]string, len(cur))
	for k, v := range cur {
		out[k] = v
	}
	return out
}

// setProviders 整体替换快照(存入拷贝,防调用方后续改入参)。
func setProviders(m map[string]string) {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	providerSnapshot.Store(cp)
}

// providersFromFleet 把 fleet active 列表转 provider→binary map。
func providersFromFleet(rows []fleetProvider) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		bin := r.BinaryName
		if bin == "" {
			bin = r.Name // 兜底:binary_name 缺省用 name
		}
		out[r.Name] = bin
	}
	return out
}
