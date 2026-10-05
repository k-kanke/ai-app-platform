// Package trace extracts the Agent's latency trace ("AAP_TRACE {json}") from a pod log.
//
// The line is printed by agent-runtime/entrypoint.sh on exit. It is advisory measurement data:
// it never decides success or failure of an operation.
package trace

import (
	"encoding/json"
	"strings"
)

const prefix = "AAP_TRACE "
const maxBytes = 4096

// Extract returns the last valid AAP_TRACE JSON object in log, or "" if there is none.
// The agent can print arbitrary text, so the line is validated as a JSON object and size-limited.
func Extract(log string) string {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		raw := strings.TrimPrefix(l, prefix)
		if len(raw) > maxBytes {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(raw), &obj) != nil {
			continue
		}
		return raw
	}
	return ""
}
