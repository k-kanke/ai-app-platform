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

func TestClassifyTask(t *testing.T) {
	cases := map[string]string{
		"fatal: repository 'https://github.com/k-kanke/nope.git/' not found":            "リポジトリを取得できませんでした",
		"fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host": "リポジトリを取得できませんでした",
		"npm ERR! code ELIFECYCLE\nnpm ERR! test failed":                                "テストが通りませんでした",
		"exceeded your current quota":                                                   "AIの利用上限",
		"something odd":                                                                 "作業が途中で止まりました",
	}
	for in, want := range cases {
		if got := ClassifyTask(in, ""); !strings.Contains(got, want) {
			t.Errorf("ClassifyTask(%q) = %q, want it to contain %q", in, got, want)
		}
	}
	// Family-facing wording must not leak into task failures.
	if got := ClassifyTask("something odd", ""); strings.Contains(got, "アプリ") {
		t.Errorf("task fallback mentions the app: %q", got)
	}
}

func TestClassifyPublish(t *testing.T) {
	cases := map[string]string{
		"publisher: FAILED: publish refused: .github/workflows/x.yml: CI":      "公開できない変更",
		"the patch does not match the approved hash":                           "承認した差分",
		"the patch does not apply to the current main: error: patch failed":    "最新のコードに適用できません",
		"push: exit status 128: remote: error: GH006: Protected branch update": "GitHub への書き込みが拒否",
		"open the pull request: github api: 422 Validation Failed":             "PR を作れませんでした",
		"something odd": "PR を作る途中で止まりました",
	}
	for in, want := range cases {
		if got := ClassifyPublish(in, ""); !strings.Contains(got, want) {
			t.Errorf("ClassifyPublish(%q) = %q, want it to contain %q", in, got, want)
		}
	}
}
