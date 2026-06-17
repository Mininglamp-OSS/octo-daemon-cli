package internal

import (
	"sync"
	"testing"
)

func TestCurrentProviders_DefaultFallback(t *testing.T) {
	s := newProviderStore()
	got := s.current()
	// 编译期 fallback 只含 claude/openclaw,不含 codex/hermes
	if _, ok := got["claude"]; !ok {
		t.Errorf("expected claude in fallback, got %v", got)
	}
	if _, ok := got["openclaw"]; !ok {
		t.Errorf("expected openclaw in fallback, got %v", got)
	}
	if _, ok := got["codex"]; ok {
		t.Errorf("codex must not be in fallback, got %v", got)
	}
	if _, ok := got["hermes"]; ok {
		t.Errorf("hermes must not be in fallback, got %v", got)
	}
}

func TestSetProviders_ReturnsCopy(t *testing.T) {
	s := newProviderStore()
	s.set(map[string]string{"claude": "claude"})
	got := s.current()
	got["mutated"] = "x" // 改返回值不能污染内部快照
	again := s.current()
	if _, ok := again["mutated"]; ok {
		t.Error("current must return a defensive copy")
	}
}

func TestSetProviders_EmptyClearsSnapshot(t *testing.T) {
	s := newProviderStore()
	s.set(map[string]string{}) // 200 空 active → 快照清空(不退化为 fallback)
	got := s.current()
	if len(got) != 0 {
		t.Errorf("expected empty snapshot after set(empty), got %v", got)
	}
}

func TestProvidersFromFleet_MapsBinaryName(t *testing.T) {
	supported := map[string]struct{}{"claude": {}, "openclaw": {}}
	in := []fleetProvider{
		{Name: "claude", BinaryName: "claude"},
		{Name: "openclaw", BinaryName: "openclaw"},
	}
	got := providersFromFleet(in, supported)
	if got["claude"] != "claude" || got["openclaw"] != "openclaw" {
		t.Errorf("unexpected map: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d", len(got))
	}
}

func TestProvidersFromFleet_BinaryNameFallsBackToName(t *testing.T) {
	supported := map[string]struct{}{"claude": {}}
	got := providersFromFleet([]fleetProvider{{Name: "claude", BinaryName: ""}}, supported)
	if got["claude"] != "claude" {
		t.Errorf("empty binary_name should fall back to name, got %v", got)
	}
}

func TestProvidersFromFleet_FiltersUnsupportedAndEmpty(t *testing.T) {
	// daemon 只支持 claude/openclaw;fleet 误标 codex active + 一行空 name → 都跳过。
	supported := map[string]struct{}{"claude": {}, "openclaw": {}}
	in := []fleetProvider{
		{Name: "claude", BinaryName: "claude"},
		{Name: "codex", BinaryName: "codex"}, // 不支持,跳过
		{Name: "", BinaryName: "x"},          // 空 name,跳过
	}
	got := providersFromFleet(in, supported)
	if len(got) != 1 {
		t.Fatalf("expected only claude, got %v", got)
	}
	if _, ok := got["codex"]; ok {
		t.Error("unsupported codex must be filtered out")
	}
}

func TestProvidersFromFleet_EmptyKeepsCaller(t *testing.T) {
	// 空列表返回空 map;调用方(daemon)决定语义(200 空→清空,失败→保留)
	got := providersFromFleet(nil, map[string]struct{}{"claude": {}})
	if got == nil {
		t.Error("providersFromFleet(nil) must return non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestSetProvidersIfNewer_StaleResponseDropped(t *testing.T) {
	s := newProviderStore()
	// 模拟两次并发刷新:seq2 后开始(更新),seq1 先开始(更旧)。
	seq1 := s.nextSeq()
	seq2 := s.nextSeq()
	// 更新的 seq2 先落地
	if !s.setIfNewer(map[string]string{"openclaw": "openclaw"}, seq2) {
		t.Fatal("newest seq must write")
	}
	// 更旧的 seq1 晚返回,必须被丢弃,不能覆盖 seq2 的结果
	if s.setIfNewer(map[string]string{"claude": "claude"}, seq1) {
		t.Error("stale seq must not overwrite newer snapshot")
	}
	got := s.current()
	if _, ok := got["openclaw"]; !ok {
		t.Errorf("expected seq2 snapshot to win, got %v", got)
	}
	if _, ok := got["claude"]; ok {
		t.Errorf("stale seq1 must not have written, got %v", got)
	}
}

// TestSetProvidersIfNewer_ConcurrentInterleaving 用 barrier 复现真实并发交错:
// 旧 seq 通过检查后、写入前,被新 seq 插队写入,旧 seq 不得覆盖新 seq。
// mu 把"比较+写"做成原子,所以无论调度顺序,最终快照都属于最大 seq。
func TestSetProvidersIfNewer_ConcurrentInterleaving(t *testing.T) {
	s := newProviderStore()
	const n = 100
	seqs := make([]uint64, n)
	for i := range seqs {
		seqs[i] = s.nextSeq()
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同时起跑,最大化交错
			s.setIfNewer(map[string]string{"p": string(rune('a' + i%26))}, seqs[i])
		}(i)
	}
	close(start)
	wg.Wait()
	// 最大 seq 是 seqs[n-1];最终 lastSeq 必须 == 它,快照属于它。
	if !s.setIfNewer(map[string]string{"final": "x"}, seqs[n-1]+1) {
		t.Fatal("a strictly greater seq must always win")
	}
	if got := s.current(); got["final"] != "x" {
		t.Errorf("expected final write to win, got %v", got)
	}
}

func TestCurrentProviders_ConcurrentSafe(t *testing.T) {
	s := newProviderStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = s.current() }()
		go func() { defer wg.Done(); s.set(map[string]string{"claude": "claude"}) }()
		go func() { defer wg.Done(); _ = DetectRuntimesFast(s.current()) }() // 覆盖真实读路径
	}
	wg.Wait() // -race 下不得 panic
}
