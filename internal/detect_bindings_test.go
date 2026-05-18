package internal

import (
	"reflect"
	"sort"
	"testing"
)

const bindingsFixture = `[
  {"agentId":"main","match":{"channel":"dmwork","accountId":"bot_a"},"description":"dmwork accountId=bot_a"},
  {"agentId":"test","match":{"channel":"dmwork","accountId":"bot_b"},"description":"dmwork accountId=bot_b"},
  {"agentId":"main","match":{"channel":"dmwork","accountId":"bot_c"},"description":"dmwork accountId=bot_c"},
  {"agentId":"caster","match":{"channel":"dmwork","accountId":"bot_d"},"description":"dmwork accountId=bot_d"}
]`

func TestParseOpenclawBindingsJSON_GroupsByAgentId(t *testing.T) {
	got, err := parseOpenclawBindingsJSON([]byte(bindingsFixture))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got["main"]) != 2 {
		t.Errorf("main should have 2 routes, got %+v", got["main"])
	}
	if len(got["test"]) != 1 || got["test"][0] != "dmwork/bot_b" {
		t.Errorf("test routes wrong: %+v", got["test"])
	}
	if len(got["caster"]) != 1 || got["caster"][0] != "dmwork/bot_d" {
		t.Errorf("caster routes wrong: %+v", got["caster"])
	}
	sort.Strings(got["main"])
	want := []string{"dmwork/bot_a", "dmwork/bot_c"}
	if !reflect.DeepEqual(got["main"], want) {
		t.Errorf("main routes wrong: got %+v, want %+v", got["main"], want)
	}
}

func TestParseOpenclawBindingsJSON_SkipsIncomplete(t *testing.T) {
	input := `[
	  {"agentId":"","match":{"channel":"dmwork","accountId":"bot_a"}},
	  {"agentId":"main","match":{"channel":"dmwork","accountId":""}},
	  {"agentId":"main","match":{"channel":"dmwork","accountId":"bot_b"}}
	]`
	got, err := parseOpenclawBindingsJSON([]byte(input))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || len(got["main"]) != 1 || got["main"][0] != "dmwork/bot_b" {
		t.Errorf("expected only main/bot_b, got %+v", got)
	}
}

func TestParseOpenclawBindingsJSON_EmptyChannelFallsBack(t *testing.T) {
	input := `[{"agentId":"main","match":{"channel":"","accountId":"bot_a"}}]`
	got, err := parseOpenclawBindingsJSON([]byte(input))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["main"][0] != "unknown/bot_a" {
		t.Errorf("expected unknown/bot_a fallback, got %+v", got["main"])
	}
}

func TestParseOpenclawBindingsJSON_EmptyArray(t *testing.T) {
	got, err := parseOpenclawBindingsJSON([]byte(`[]`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %+v", got)
	}
}

func TestParseOpenclawBindingsJSON_Malformed(t *testing.T) {
	if _, err := parseOpenclawBindingsJSON([]byte(`not json`)); err == nil {
		t.Error("expected error on malformed input")
	}
}

func TestMergeBindingsIntoAgents(t *testing.T) {
	agents := []AgentEntry{
		{ID: "main", Bindings: 2},
		{ID: "test", Bindings: 1},
		{ID: "unused", Bindings: 0},
	}
	bindings := map[string][]string{
		"main": {"dmwork/bot_a", "dmwork/bot_c"},
		"test": {"dmwork/bot_b"},
	}
	mergeBindingsIntoAgents(agents, bindings)
	if len(agents[0].Routes) != 2 {
		t.Errorf("main routes wrong: %+v", agents[0].Routes)
	}
	if len(agents[1].Routes) != 1 {
		t.Errorf("test routes wrong: %+v", agents[1].Routes)
	}
	if len(agents[2].Routes) != 0 {
		t.Errorf("unused should have no routes, got %+v", agents[2].Routes)
	}
}
