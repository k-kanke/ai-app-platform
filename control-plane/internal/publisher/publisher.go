// Package publisher turns an approved patch into a branch and a pull request.
//
// It runs in the Publisher Job, the only pod that holds a GitHub token, and it never runs an LLM or the
// agent's code. It does not open the Implementer's workspace (the agent controls its .git): it downloads
// the approved patch from the Control Plane, checks the patch hash and content, applies it to a FRESH
// clone, commits on an agent/<task> branch, pushes (never to the default branch, never forced), and opens
// a regular pull request. docs/multi-agent-permissions.md §5.3.
package publisher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/github"
	"github.com/k-kanke/ai-app-platform/control-plane/internal/patch"
)

type Config struct {
	Repo, TaskID string
	PatchSHA     string // the hash the human approved
	Token        string // GitHub installation token (1 hour, this repository only)
	GitBase      string // "" = https://github.com
	APIBase      string // "" = https://api.github.com
	PRTitle      string
	PRBody       string

	ControlPlaneURL string
	PublisherToken  string // authorises GET patch and POST published for this task only
	WorkDir         string
	MaxPatchBytes   int
	HTTP            *http.Client
	Git             string // git binary ("" = git)
	Log             func(format string, a ...any)
}

// Result is what the Publisher reports back.
type Result struct {
	Branch   string `json:"branch"`
	PRURL    string `json:"prUrl"`
	PRNumber int    `json:"prNumber"`
}

func (c *Config) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

func (c *Config) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Branch is where a task's change is pushed. It is always under agent/.
func Branch(taskID string) string { return "agent/" + taskID }

// Check applies the publish policy to a patch summary. A non-nil error is a refusal with a reason a
// person can read; the policy is deliberately stricter than the approval screen's warnings.
func Check(s patch.Summary) error {
	if len(s.Files) == 0 {
		return errors.New("the patch changes no file")
	}
	var why []string
	for _, f := range s.Files {
		switch {
		case f.Risk != "":
			why = append(why, f.Path+": "+f.Risk)
		case f.Binary:
			why = append(why, f.Path+": バイナリの変更")
		case f.ModeSet:
			why = append(why, f.Path+": 実行権限の変更")
		}
	}
	if s.Blocked {
		why = append(why, "公開できないパスを含む")
	}
	if len(why) > 0 {
		sort.Strings(why)
		return fmt.Errorf("publish refused: %s", strings.Join(why, "; "))
	}
	return nil
}

// Run publishes the approved patch.
func Run(ctx context.Context, cfg Config) (Result, error) {
	var res Result
	if cfg.WorkDir == "" {
		d, err := os.MkdirTemp("", "publish-")
		if err != nil {
			return res, err
		}
		defer os.RemoveAll(d)
		cfg.WorkDir = d
	}
	if cfg.MaxPatchBytes <= 0 {
		cfg.MaxPatchBytes = 1 << 20
	}

	data, err := fetchPatch(ctx, &cfg)
	if err != nil {
		return res, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != cfg.PatchSHA {
		return res, fmt.Errorf("the patch does not match the approved hash (approved %s, got %s)", cfg.PatchSHA, got)
	}
	summary := patch.Summarize(data)
	if err := Check(summary); err != nil {
		return res, err
	}
	cfg.logf("patch %s verified: %d file(s), +%d -%d", cfg.PatchSHA[:12], len(summary.Files), summary.Added, summary.Deleted)

	repoDir := filepath.Join(cfg.WorkDir, "repo")
	patchFile := filepath.Join(cfg.WorkDir, "approved.patch")
	if err := os.WriteFile(patchFile, data, 0o600); err != nil {
		return res, err
	}
	g := &gitRunner{cfg: &cfg, dir: cfg.WorkDir}
	cloneURL := strings.TrimRight(orDefault(cfg.GitBase, "https://github.com"), "/") + "/" + cfg.Repo + ".git"
	if _, err := g.run(ctx, true, "clone", "--depth", "1", "--no-tags", "--", cloneURL, repoDir); err != nil {
		return res, fmt.Errorf("clone: %w", err)
	}
	g.dir = repoDir
	base, err := g.run(ctx, false, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(base) == "" || strings.TrimSpace(base) == "HEAD" {
		return res, fmt.Errorf("cannot tell the default branch: %v", err)
	}
	base = strings.TrimSpace(base)
	res.Branch = Branch(cfg.TaskID)
	if res.Branch == base {
		return res, errors.New("refusing to push to the default branch")
	}

	if _, err := g.run(ctx, false, "apply", "--index", "--whitespace=nowarn", patchFile); err != nil {
		return res, fmt.Errorf("the patch does not apply to the current %s: %w", base, err)
	}
	// What actually changed must be exactly what was approved and checked.
	names, err := g.run(ctx, false, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return res, err
	}
	if err := sameFiles(summary, names); err != nil {
		return res, err
	}
	if _, err := g.run(ctx, false, "-c", "user.name=genkan agent", "-c", "user.email=agent@users.noreply.github.com",
		"-c", "commit.gpgsign=false", "checkout", "-b", res.Branch); err != nil {
		return res, err
	}
	if _, err := g.run(ctx, false, "-c", "user.name=genkan agent", "-c", "user.email=agent@users.noreply.github.com",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", cfg.PRTitle, "-m", "genkan task "+cfg.TaskID); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}

	api := github.Client{Token: cfg.Token, APIBase: cfg.APIBase, HTTP: cfg.HTTP}
	// A retry after a partial failure finds its own branch (and PR) instead of failing on "already exists".
	remote, _ := g.run(ctx, true, "ls-remote", "--heads", "origin", "refs/heads/"+res.Branch)
	if strings.TrimSpace(remote) == "" {
		if _, err := g.run(ctx, true, "push", "origin", "HEAD:refs/heads/"+res.Branch); err != nil {
			return res, fmt.Errorf("push: %w", err)
		}
		cfg.logf("pushed %s", res.Branch)
	} else {
		cfg.logf("branch %s already exists on the remote; reusing it", res.Branch)
		if pr, found, err := api.FindOpenPR(ctx, cfg.Repo, res.Branch); err == nil && found {
			res.PRURL, res.PRNumber = pr.HTMLURL, pr.Number
			return res, report(ctx, &cfg, res)
		}
	}
	pr, err := api.CreatePR(ctx, cfg.Repo, cfg.PRTitle, cfg.PRBody, res.Branch, base)
	if err != nil {
		return res, fmt.Errorf("open the pull request: %w", err)
	}
	res.PRURL, res.PRNumber = pr.HTMLURL, pr.Number
	cfg.logf("opened PR #%d %s", pr.Number, pr.HTMLURL)
	return res, report(ctx, &cfg, res)
}

func orDefault(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

func sameFiles(s patch.Summary, nameZ string) error {
	want := map[string]bool{}
	for _, f := range s.Files {
		want[f.Path] = true
	}
	got := map[string]bool{}
	for _, n := range strings.Split(nameZ, "\x00") {
		if n != "" {
			got[n] = true
		}
	}
	for n := range got {
		if !want[n] {
			return fmt.Errorf("the applied change touches %q, which the approved patch does not list", n)
		}
	}
	for n := range want {
		if !got[n] {
			return fmt.Errorf("the approved patch lists %q but applying it did not change it", n)
		}
	}
	return nil
}

func fetchPatch(ctx context.Context, cfg *Config) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(cfg.ControlPlaneURL, "/")+"/internal/v1/tasks/"+cfg.TaskID+"/patch", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.PublisherToken)
	resp, err := cfg.httpc().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download the patch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download the patch: control plane answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(cfg.MaxPatchBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > cfg.MaxPatchBytes {
		return nil, errors.New("the patch is larger than the limit")
	}
	return data, nil
}

func report(ctx context.Context, cfg *Config, r Result) error {
	b, _ := json.Marshal(r)
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.ControlPlaneURL, "/")+"/internal/v1/tasks/"+cfg.TaskID+"/published", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+cfg.PublisherToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.httpc().Do(req)
	if err != nil {
		return fmt.Errorf("report the pull request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("report the pull request: control plane answered %d", resp.StatusCode)
	}
	return nil
}

// gitRunner runs git with hooks, system and global config switched off. The token is passed as an HTTP
// header for network commands only; it is never written into a remote URL or the repository config.
type gitRunner struct {
	cfg *Config
	dir string
}

func (g *gitRunner) run(ctx context.Context, network bool, args ...string) (string, error) {
	pre := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.ext.allow=never"}
	if network && g.cfg.Token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.cfg.Token))
		pre = append(pre, "-c", "http.extraheader=Authorization: Basic "+basic)
	}
	bin := orDefault(g.cfg.Git, "git")
	cmd := exec.CommandContext(ctx, bin, append(pre, args...)...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ASKPASS=true", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", firstArg(args), err, g.scrub(strings.TrimSpace(errb.String())))
	}
	return out.String(), nil
}

func firstArg(a []string) string {
	for i := 0; i < len(a); i++ {
		if a[i] == "-c" {
			i++
			continue
		}
		return a[i]
	}
	return ""
}

// scrub removes the token (and its header form) from git's error text before it is logged or stored.
func (g *gitRunner) scrub(s string) string {
	if g.cfg.Token == "" {
		return s
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.cfg.Token))
	return strings.NewReplacer(g.cfg.Token, "***", basic, "***").Replace(s)
}
