package internal

import (
	"strconv"
	"strings"
)

// isVersionOlder reports whether current < latest using the same semantics as
// the server's modules/runtime/api.go isVersionOlder:
//   - strip leading "v"
//   - strip everything after the first "-" or "+" (pre-release / build metadata)
//   - split remainder on "." and compare element-wise as integers
//   - non-numeric on either side → false (do not claim older; avoid false fails)
//   - "dev" / "unknown" / "" on current → older than any real version
//
// Used on daemon side to decide if a post-upgrade version actually reached
// the task's TargetVersion; keeps daemon's judgement aligned with server's
// close-out matching so we don't report failed for versions the server would
// accept.
func isVersionOlder(current, latest string) bool {
	if current == "dev" || current == "unknown" || current == "" {
		return latest != "" && latest != "dev" && latest != "unknown"
	}

	parse := func(v string) []int {
		v = strings.TrimPrefix(v, "v")
		for _, sep := range []string{"-", "+"} {
			if idx := strings.Index(v, sep); idx > 0 {
				v = v[:idx]
			}
		}
		parts := strings.Split(v, ".")
		nums := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			nums = append(nums, n)
		}
		return nums
	}

	c := parse(current)
	l := parse(latest)
	if c == nil || l == nil {
		return false
	}

	for i := 0; i < len(c) && i < len(l); i++ {
		if c[i] < l[i] {
			return true
		}
		if c[i] > l[i] {
			return false
		}
	}
	return len(c) < len(l)
}
