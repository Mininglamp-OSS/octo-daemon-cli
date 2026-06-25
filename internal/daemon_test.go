package internal

import (
	"testing"
)

func TestRuntimesChanged_NoChange(t *testing.T) {
	a := []RuntimeInfo{
		{Provider: "claude", Version: "2.1.0", Status: "online"},
		{Provider: "openclaw", Version: "2026.4.21", Status: "online", Agents: []AgentEntry{{ID: "main", Bindings: 5}}},
	}
	b := []RuntimeInfo{
		{Provider: "claude", Version: "2.1.0", Status: "online"},
		{Provider: "openclaw", Version: "2026.4.21", Status: "online", Agents: []AgentEntry{{ID: "main", Bindings: 5}}},
	}
	if runtimesChanged(a, b) {
		t.Fatal("expected no change")
	}
}

func TestRuntimesChanged_VersionChange(t *testing.T) {
	a := []RuntimeInfo{{Provider: "claude", Version: "2.1.0", Status: "online"}}
	b := []RuntimeInfo{{Provider: "claude", Version: "2.2.0", Status: "online"}}
	if !runtimesChanged(a, b) {
		t.Fatal("expected change on version")
	}
}

func TestRuntimesChanged_StatusChange(t *testing.T) {
	a := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online"}}
	b := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "offline"}}
	if !runtimesChanged(a, b) {
		t.Fatal("expected change on status")
	}
}

func TestRuntimesChanged_AgentAdded(t *testing.T) {
	a := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Agents: []AgentEntry{{ID: "main", Bindings: 5}}}}
	b := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Agents: []AgentEntry{{ID: "main", Bindings: 5}, {ID: "test", Bindings: 2}}}}
	if !runtimesChanged(a, b) {
		t.Fatal("expected change on agent count")
	}
}

func TestRuntimesChanged_BindingsChange(t *testing.T) {
	a := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Agents: []AgentEntry{{ID: "main", Bindings: 5}}}}
	b := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Agents: []AgentEntry{{ID: "main", Bindings: 10}}}}
	if !runtimesChanged(a, b) {
		t.Fatal("expected change on bindings")
	}
}

func TestRuntimesChanged_PluginVersionChange(t *testing.T) {
	a := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Plugins: []PluginInfo{{Name: "octo", Version: "0.6.1"}}}}
	b := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Plugins: []PluginInfo{{Name: "octo", Version: "0.7.0"}}}}
	if !runtimesChanged(a, b) {
		t.Fatal("expected change on plugin version")
	}
}

func TestRuntimesChanged_PluginAdded(t *testing.T) {
	a := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Plugins: []PluginInfo{{Name: "octo", Version: "0.6.1"}}}}
	b := []RuntimeInfo{{Provider: "openclaw", Version: "1.0", Status: "online", Plugins: []PluginInfo{{Name: "octo", Version: "0.6.1"}, {Name: "openclaw-lark", Version: "1.0.0"}}}}
	if !runtimesChanged(a, b) {
		t.Fatal("expected change on plugin count")
	}
}

func TestRuntimesChanged_ProviderAdded(t *testing.T) {
	a := []RuntimeInfo{{Provider: "claude", Version: "2.1.0", Status: "online"}}
	b := []RuntimeInfo{{Provider: "claude", Version: "2.1.0", Status: "online"}, {Provider: "openclaw", Version: "0.1.0", Status: "online"}}
	if !runtimesChanged(a, b) {
		t.Fatal("expected change on provider count")
	}
}

func TestComponentsChanged_NoChange(t *testing.T) {
	a := []DeviceComponent{
		{ComponentKey: "@mininglamp-oss/octo-cli", Version: "1.0.10"},
		{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.4"},
	}
	b := []DeviceComponent{
		{ComponentKey: "@mininglamp-oss/octo-cli", Version: "1.0.10"},
		{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.4"},
	}
	if componentsChanged(a, b) {
		t.Fatal("expected no change")
	}
}

func TestComponentsChanged_VersionChange(t *testing.T) {
	a := []DeviceComponent{{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.4"}}
	b := []DeviceComponent{{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.5"}}
	if !componentsChanged(a, b) {
		t.Fatal("expected change on version")
	}
}

func TestComponentsChanged_Installed(t *testing.T) {
	a := []DeviceComponent{{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.5"}}
	b := []DeviceComponent{
		{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.5"},
		{ComponentKey: "@mininglamp-oss/octo-cli", Version: "1.0.10"},
	}
	if !componentsChanged(a, b) {
		t.Fatal("expected change on install (count up)")
	}
}

func TestComponentsChanged_Uninstalled(t *testing.T) {
	a := []DeviceComponent{
		{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.5"},
		{ComponentKey: "@mininglamp-oss/octo-cli", Version: "1.0.10"},
	}
	b := []DeviceComponent{{ComponentKey: "@mininglamp-oss/octo-daemon", Version: "0.0.5"}}
	if !componentsChanged(a, b) {
		t.Fatal("expected change on uninstall (count down)")
	}
}
