package internal

import (
	"encoding/json"
	"testing"
)

func TestExtractJSONArray_PrettyPrinted(t *testing.T) {
	input := `[dmwork] registering before_prompt_build hook
[
  {
    "id": "main",
    "bindings": 10,
    "isDefault": true
  }
]
[dmwork] registering before_prompt_build hook`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 element, got %d", len(arr))
	}
}

func TestExtractJSONArray_SingleLine(t *testing.T) {
	input := `[plugins] loading...
[{"id":"main","bindings":2,"isDefault":true}]
done`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 element, got %d", len(arr))
	}
}

func TestExtractJSONArray_CleanJSON(t *testing.T) {
	input := `[{"id":"main"},{"id":"test"}]`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(arr))
	}
}

func TestExtractJSONArray_Empty(t *testing.T) {
	result := extractJSONArray([]byte("no json here"))
	if result != nil {
		t.Fatalf("expected nil, got %s", string(result))
	}
}

func TestExtractJSONArray_PrefixWithBracket(t *testing.T) {
	input := `[plugins] octo loaded
[dmwork] hook registered
[
  {"id": "main", "bindings": 5, "isDefault": true}
]`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}
