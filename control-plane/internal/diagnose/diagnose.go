// Package diagnose turns an Agent's raw failure (log tail, job reason) into a
// message a non-engineer can act on. The raw text stays available to operators.
package diagnose

import (
	"regexp"
	"strings"
)

type rule struct {
	re  *regexp.Regexp
	msg string
}

// Order matters: the first match wins, most specific first.
var rules = []rule{
	{regexp.MustCompile(`(?i)spending cap|exceeded your current quota|RESOURCE_EXHAUSTED|status:?\s*429|rate.?limit`),
		"AIの利用上限に達しています。しばらく待つか、管理者が上限を見直す必要があります。"},
	{regexp.MustCompile(`(?i)API key not valid|API_KEY_INVALID|PERMISSION_DENIED|status:?\s*40[13]|_API_KEY is not set`),
		"AIを使うための鍵(キー)に問題があります。管理者に連絡してください。"},
	{regexp.MustCompile(`(?i)DeadlineExceeded|exit(ed)? (with )?(code )?124|timed out`),
		"時間がかかりすぎて止まりました。依頼を小さく分けて、もう一度お試しください。"},
	{regexp.MustCompile(`(?i)ECONNREFUSED|ENOTFOUND|EAI_AGAIN|ETIMEDOUT|fetch failed|socket hang up|status:?\s*50[0-9]`),
		"AIにつながりませんでした。少し待ってから、もう一度お試しください。"},
	{regexp.MustCompile(`(?i)# fail [1-9]|not ok \d+|npm ERR! .*test|確認テストに失敗`),
		"作ったアプリの動作確認(テスト)が通りませんでした。言い方を変えて、もう一度お試しください。"},
}

const fallback = "うまく作れませんでした。言い方を変えて、もう一度お試しください。"

// Classify returns the user-facing message for an Agent failure.
func Classify(logTail, reason string) string {
	text := logTail + "\n" + reason
	for _, r := range rules {
		if r.re.MatchString(text) {
			return r.msg
		}
	}
	return fallback
}

// Tail keeps the last n lines and at most max bytes, for storing as operator detail.
func Tail(s string, n, max int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// taskRules are checked before the shared ones for repository tasks, whose reader is the owner
// (not a family member) and whose failures include cloning and the repository's own tests.
var taskRules = []rule{
	{regexp.MustCompile(`(?i)repository .*not found|could not read from remote|unable to access|fatal: .*clone|Authentication failed|Could not resolve host`),
		"リポジトリを取得できませんでした。名前が違うか、非公開の可能性があります。"},
	{regexp.MustCompile(`(?i)# fail [1-9]|not ok \d+|npm ERR! .*test|Tests?:\s+\d+ failed|FAIL\s`),
		"テストが通りませんでした。詳しい内容はログで確認できます。"},
}

const taskFallback = "作業が途中で止まりました。詳しい内容はログで確認できます。"

// ClassifyTask is Classify for a repository task's Implementer.
func ClassifyTask(logTail, reason string) string {
	text := logTail + "\n" + reason
	for _, r := range taskRules {
		if r.re.MatchString(text) {
			return r.msg
		}
	}
	for _, r := range rules[:4] { // quota, key, timeout, connectivity: wording applies to any agent
		if r.re.MatchString(text) {
			return r.msg
		}
	}
	return taskFallback
}
