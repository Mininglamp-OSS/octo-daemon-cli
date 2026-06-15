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
	in := []fleetProvider{
		{Name: "claude", BinaryName: "claude"},
		{Name: "openclaw", BinaryName: "openclaw"},
	}
	got := providersFromFleet(in)
	if got["claude"] != "claude" || got["openclaw"] != "openclaw" {
		t.Errorf("unexpected map: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d", len(got))
	}
}

func TestProvidersFromFleet_EmptyKeepsCaller(t *testing.T) {
	// 空列表返回空 map;调用方(daemon)决定语义(200 空→清空,失败→保留)
	got := providersFromFleet(nil)
	if got == nil {
		t.Error("providersFromFleet(nil) must return non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
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
