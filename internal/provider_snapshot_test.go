package internal

import (
	"sync"
	"testing"
)

func TestCurrentProviders_DefaultFallback(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	got := currentProviders()
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
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	setProviders(map[string]string{"claude": "claude"})
	got := currentProviders()
	got["mutated"] = "x" // 改返回值不能污染内部快照
	again := currentProviders()
	if _, ok := again["mutated"]; ok {
		t.Error("currentProviders must return a defensive copy")
	}
}

func TestSetProviders_EmptyClearsSnapshot(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	setProviders(map[string]string{}) // 200 空 active → 快照清空(不退化为 fallback)
	got := currentProviders()
	if len(got) != 0 {
		t.Errorf("expected empty snapshot after setProviders(empty), got %v", got)
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
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	// 模拟两次并发刷新:seq2 后开始(更新),seq1 先开始(更旧)。
	seq1 := nextRefreshSeq()
	seq2 := nextRefreshSeq()
	// 更新的 seq2 先落地
	if !setProvidersIfNewer(map[string]string{"openclaw": "openclaw"}, seq2) {
		t.Fatal("newest seq must write")
	}
	// 更旧的 seq1 晚返回,必须被丢弃,不能覆盖 seq2 的结果
	if setProvidersIfNewer(map[string]string{"claude": "claude"}, seq1) {
		t.Error("stale seq must not overwrite newer snapshot")
	}
	got := currentProviders()
	if _, ok := got["openclaw"]; !ok {
		t.Errorf("expected seq2 snapshot to win, got %v", got)
	}
	if _, ok := got["claude"]; ok {
		t.Errorf("stale seq1 must not have written, got %v", got)
	}
}

func TestCurrentProviders_ConcurrentSafe(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = currentProviders() }()
		go func() { defer wg.Done(); setProviders(map[string]string{"claude": "claude"}) }()
		go func() { defer wg.Done(); _ = DetectRuntimesFast() }() // 覆盖真实读路径
	}
	wg.Wait() // -race 下不得 panic
}
