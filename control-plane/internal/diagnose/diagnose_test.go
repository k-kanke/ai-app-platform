package diagnose

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct{ name, log, reason, want string }{
		{"spend cap", "Attempt 10 failed: Your project has exceeded its monthly spending cap.\nstatus: 429", "BackoffLimitExceeded", "利用上限"},
		{"quota", "RESOURCE_EXHAUSTED: quota", "", "利用上限"},
		{"bad key", "API key not valid. Please pass a valid API key", "", "鍵"},
		{"missing key", "GEMINI_API_KEY: GEMINI_API_KEY is not set (Agent Secret)", "", "鍵"},
		{"deadline", "", "DeadlineExceeded: Job was active longer than specified deadline", "時間がかかりすぎ"},
		{"network", "TypeError: fetch failed\n ENOTFOUND generativelanguage.googleapis.com", "", "つながりませんでした"},
		{"tests", "not ok 3 - POST /api/items\n# fail 2", "", "動作確認"},
		{"unknown", "something odd", "BackoffLimitExceeded", "うまく作れませんでした"},
	}
	for _, c := range cases {
		if got := Classify(c.log, c.reason); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, got, c.want)
		}
	}
	// A quota error must win over generic network wording in the same log.
	if got := Classify("fetch failed\nstatus: 429 spending cap", ""); !strings.Contains(got, "利用上限") {
		t.Errorf("quota should take priority, got %q", got)
	}
}

func TestTail(t *testing.T) {
	in := "a\nb\nc\nd\ne\n"
	if got := Tail(in, 2, 100); got != "d\ne" {
		t.Errorf("got %q", got)
	}
	if got := Tail(strings.Repeat("x", 50), 5, 10); len(got) != 10 {
		t.Errorf("max bytes not applied: %d", len(got))
	}
}
