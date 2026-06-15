package internal

import (
	"log"
	"sync"
	"sync/atomic"
)

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

// providerRefreshSeq 是刷新版本门禁:多个挂点并发 refreshProviders 时,各自
// 在发请求前 Add(1) 拿一个递增序号。写快照时在 refreshMu 临界区内比较序号,
// 只有"最新开始"的那次刷新允许写,避免较早发出但较晚返回的响应覆盖更新的快照。
var providerRefreshSeq atomic.Uint64

// refreshMu 保护 setProvidersIfNewer 的"比较序号 + 写快照"为原子操作。
// 否则 Load 检查与 Store 之间会有 TOCTOU 窗口:seq2 通过检查 → seq3 写入 →
// seq2 才 Store 覆盖掉 seq3。
var refreshMu sync.Mutex

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
// 快照与序号在同一 refreshMu 临界区内重置,与 setProvidersIfNewer 保持一致。
func resetProvidersForTest() {
	cp := make(map[string]string, len(fallbackProviders))
	for k, v := range fallbackProviders {
		cp[k] = v
	}
	refreshMu.Lock()
	providerSnapshot.Store(cp)
	lastWrittenSeq = 0
	providerRefreshSeq.Store(0)
	refreshMu.Unlock()
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

// lastWrittenSeq 是已写入快照的最大刷新序号(refreshMu 保护)。
var lastWrittenSeq uint64

// nextRefreshSeq 在每次 refresh 发请求前调用,返回本次刷新的版本号。
func nextRefreshSeq() uint64 {
	return providerRefreshSeq.Add(1)
}

// setProvidersIfNewer 在 refreshMu 临界区内比较序号:仅当 mySeq 大于已写入的
// 最大序号时才写快照,并推进 lastWrittenSeq。并发刷新下,较早开始(mySeq 较小)
// 但较晚返回的响应会被丢弃,保证写入按 seq 单调推进、不被旧响应覆盖。
// 返回是否实际写入。
func setProvidersIfNewer(m map[string]string, mySeq uint64) bool {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if mySeq <= lastWrittenSeq {
		return false // 已有同序或更新的刷新写过,放弃
	}
	lastWrittenSeq = mySeq
	setProviders(m)
	return true
}

// providersFromFleet 把 fleet active 列表转 provider→binary map。
//
// 按 daemon 本地支持集(supported,即已注册 adapter 的 kind)过滤:fleet 若
// 误配/迁移残留把 codex/hermes 等本 daemon 已不支持的 provider 标 active,
// 直接 warn + 跳过 —— 否则 daemon 会去 LookPath/探测/注册一个没有 adapter
// 的 provider,后续 provision/task/upgrade 全失败(codex MAJOR)。空 name 也跳过。
func providersFromFleet(rows []fleetProvider, supported map[string]struct{}) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Name == "" {
			log.Printf("[WARN] fleet provider with empty name skipped")
			continue
		}
		if _, ok := supported[r.Name]; !ok {
			log.Printf("[WARN] fleet provider %q not supported by this daemon, skipped", r.Name)
			continue
		}
		bin := r.BinaryName
		if bin == "" {
			bin = r.Name // 兜底:binary_name 缺省用 name
		}
		out[r.Name] = bin
	}
	return out
}
