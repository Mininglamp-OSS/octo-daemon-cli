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

// fallbackProviders 是编译期兜底:fleet 不可达 / 老 fleet 无端点时使用,也是
// 无 daemon 上下文的本地 detect(status 命令)默认集。本期只放行 claude +
// openclaw(去掉 codex/hermes)。
var fallbackProviders = map[string]string{
	"claude":   "claude",
	"openclaw": "openclaw",
}

// providerStore 持有一个 daemon(单 space)的 provider→binary 快照。
//
// 每个 Daemon 各持一个实例:multi-profile 单进程 fan-out 下,supervisor 为每个
// space 并发跑 refreshProviders。若共用包级全局快照,不同 space 的刷新会互相
// 覆盖(A space 的 active 集盖掉 B space 的),DetectRuntimesFast 又从同一全局
// 读,导致某 space 按另一 space 的 provider 集注册/漏注册。per-Daemon 隔离消除
// 这种跨 space 污染。
type providerStore struct {
	// snapshot 存不可变的 provider→binary map,用 atomic.Value 整体替换,避免
	// detect / register / slow-detect 并发读写 Go map 触发 concurrent map 崩溃。
	snapshot   atomic.Value // map[string]string
	refreshSeq atomic.Uint64
	// mu 保护 setIfNewer 的"比较序号 + 写快照"为原子操作,否则 Load 检查与
	// Store 之间有 TOCTOU 窗口:seq2 通过检查 → seq3 写入 → seq2 才 Store 覆盖。
	mu      sync.Mutex
	lastSeq uint64 // 已写入快照的最大刷新序号(mu 保护)
}

// newProviderStore 返回一个快照初始化为编译期 fallback 的 store。
func newProviderStore() *providerStore {
	s := &providerStore{}
	s.reset()
	return s
}

// reset 把快照重置为编译期 fallback(构造 + 测试用)。
func (s *providerStore) reset() {
	s.mu.Lock()
	s.set(fallbackProviders)
	s.lastSeq = 0
	s.refreshSeq.Store(0)
	s.mu.Unlock()
}

// current 返回当前快照的防御性拷贝(调用方改返回值不污染内部)。
func (s *providerStore) current() map[string]string {
	cur, _ := s.snapshot.Load().(map[string]string)
	out := make(map[string]string, len(cur))
	for k, v := range cur {
		out[k] = v
	}
	return out
}

// set 整体替换快照(存入拷贝,防调用方后续改入参)。
func (s *providerStore) set(m map[string]string) {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	s.snapshot.Store(cp)
}

// nextSeq 在每次 refresh 发请求前调用,返回本次刷新的版本号。
func (s *providerStore) nextSeq() uint64 {
	return s.refreshSeq.Add(1)
}

// setIfNewer 在 mu 临界区内比较序号:仅当 mySeq 大于已写入的最大序号时才写
// 快照,并推进 lastSeq。并发刷新下,较早开始(mySeq 较小)但较晚返回的响应会
// 被丢弃,保证写入按 seq 单调推进、不被旧响应覆盖。返回是否实际写入。
func (s *providerStore) setIfNewer(m map[string]string, mySeq uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mySeq <= s.lastSeq {
		return false // 已有同序或更新的刷新写过,放弃
	}
	s.lastSeq = mySeq
	s.set(m)
	return true
}

// providersFromFleet 把 fleet active 列表转 provider→binary map。
//
// 按 daemon 本地支持集(supported,即已注册 adapter 的 kind)过滤:fleet 若
// 误配/迁移残留把 codex/hermes 等本 daemon 已不支持的 provider 标 active,
// 直接 warn + 跳过 —— 否则 daemon 会去 LookPath/探测/注册一个没有 adapter
// 的 provider,后续 provision/task/upgrade 全失败。空 name 也跳过。
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
