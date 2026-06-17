package internal

import "testing"

func TestPluginUpgradeCommand_Octo(t *testing.T) {
	bin, args, ok := pluginUpgradeCommand("octo", "1.2.3")
	if !ok {
		t.Fatal("octo must be a supported plugin component")
	}
	if bin != "npx" {
		t.Errorf("bin = %q, want npx", bin)
	}
	// openclaw's installer always pulls latest; the target version is not passed.
	want := []string{"-y", "create-openclaw-octo", "install"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestPluginUpgradeCommand_CcOcto(t *testing.T) {
	bin, args, ok := pluginUpgradeCommand("cc-octo", "1.2.3")
	if !ok {
		t.Fatal("cc-octo must be a supported plugin component")
	}
	if bin != "cc-channel-octo" {
		t.Errorf("bin = %q, want cc-channel-octo", bin)
	}
	want := []string{"upgrade", "1.2.3"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestPluginUpgradeCommand_CcOcto_NoVersion(t *testing.T) {
	_, args, ok := pluginUpgradeCommand("cc-octo", "")
	if !ok {
		t.Fatal("cc-octo with empty target must still be supported")
	}
	// Empty target → bare `upgrade` (cc-channel-octo defaults to @latest).
	want := []string{"upgrade"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestPluginUpgradeCommand_Unknown(t *testing.T) {
	if _, _, ok := pluginUpgradeCommand("octo-daemon", ""); ok {
		t.Error("octo-daemon is not a plugin component")
	}
	if _, _, ok := pluginUpgradeCommand("claude", ""); ok {
		t.Error("claude is a provider component, not a plugin")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCcOctoNpmInstallArgs(t *testing.T) {
	if got := ccOctoNpmInstallArgs("1.2.3"); !equalStrings(got, []string{"install", "-g", "@mininglamp-oss/cc-channel-octo@1.2.3"}) {
		t.Errorf("with version: %v", got)
	}
	if got := ccOctoNpmInstallArgs(""); !equalStrings(got, []string{"install", "-g", "@mininglamp-oss/cc-channel-octo@latest"}) {
		t.Errorf("empty → latest: %v", got)
	}
}
