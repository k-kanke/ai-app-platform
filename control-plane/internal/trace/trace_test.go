package trace

import (
	"strings"
	"testing"
)

func TestExtractTakesTheLastValidTraceLine(t *testing.T) {
	log := "== agent=gemini\nsome output\nAAP_TRACE {\"v\":1,\"rc\":1}\nmore\nAAP_TRACE {\"v\":1,\"rc\":0,\"writes\":4}\n"
	if got := Extract(log); got != `{"v":1,"rc":0,"writes":4}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractIgnoresGarbageAndOversizedLines(t *testing.T) {
	cases := map[string]string{
		"no trace at all":                      "hello\nworld\n",
		"truncated json (cut by a log tail)":   "AAP_TRACE {\"v\":1,\"rc\":0,\"t_entry\":17",
		"not an object":                        "AAP_TRACE [1,2,3]",
		"agent printed a fake prefix mid-line": "echo AAP_TRACE {\"v\":1}",
		"oversized":                            "AAP_TRACE {\"x\":\"" + strings.Repeat("a", 5000) + "\"}",
	}
	for name, log := range cases {
		if got := Extract(log); got != "" {
			t.Errorf("%s: want empty, got %q", name, got)
		}
	}
}

func TestExtractFallsBackToAnEarlierValidLine(t *testing.T) {
	log := "AAP_TRACE {\"v\":1,\"rc\":0}\nAAP_TRACE {broken"
	if got := Extract(log); got != `{"v":1,"rc":0}` {
		t.Fatalf("got %q", got)
	}
}
