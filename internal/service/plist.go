package service

import (
	"fmt"
	"strings"
)

// xmlEscape escapes &, <, >, ", ' for use inside a plist <string>.
// plist is XML; ProgramArguments is an array so no arg-splitting ambiguity —
// we only need to make the content safe as XML text.
func xmlEscape(s string) string {
	// Do & first so subsequent escaped sequences don't get double-escaped.
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

// PlistRenderArgs are the fields consumed by the launchd plist template.
type PlistRenderArgs struct {
	WrapperScript string
	EnvFile       string
	ExecPath      string
	ConfigPath    string
	DataDir       string
	StdoutLog     string
	StderrLog     string
}

// RenderPlist produces the launchd plist XML. ThrottleInterval=10s and
// KeepAlive.SuccessfulExit=false match plan §一 decisions (exit 0 / mapped
// 2/78 → 0 mean "don't restart"; 75 / 1 stay non-zero and trigger restart).
func RenderPlist(a PlistRenderArgs) string {
	argv := []string{
		a.WrapperScript,
		a.EnvFile,
		a.ExecPath,
		"start",
		"--config",
		a.ConfigPath,
	}
	var args strings.Builder
	for _, v := range argv {
		fmt.Fprintf(&args, "    <string>%s</string>\n", xmlEscape(v))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`,
		ServiceLabel,
		args.String(),
		xmlEscape(a.DataDir),
		xmlEscape(a.StdoutLog),
		xmlEscape(a.StderrLog),
	)
}
