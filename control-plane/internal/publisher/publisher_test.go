package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-kanke/ai-app-platform/control-plane/internal/patch"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// world is a bare "GitHub" with one repository, a fake API and a fake Control Plane.
type world struct {
	t         *testing.T
	base      string // file:// base of the bare repositories
	bare      string
	cp, api   *httptest.Server
	patchData []byte
	published []Result
	prs       map[string]int // branch -> PR number already opened
	created   []map[string]any
	nextPR    int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{t: t, base: "file://" + root, bare: filepath.Join(root, "k-kanke", "genkan.git"), prs: map[string]int{}, nextPR: 41}
	os.MkdirAll(w.bare, 0o755)
	git(t, w.bare, "init", "--bare", "-q", "-b", "main")
	seed := t.TempDir()
	git(t, seed, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "package-lock.json"), []byte("{}\n"), 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "init")
	git(t, seed, "remote", "add", "origin", w.bare)
	git(t, seed, "push", "-q", "origin", "main")

	w.cp = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pub-token" {
			rw.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/v1/tasks/t1/patch":
			rw.Write(w.patchData)
		case "POST /internal/v1/tasks/t1/published":
			var res Result
			json.NewDecoder(r.Body).Decode(&res)
			w.published = append(w.published, res)
			rw.WriteHeader(204)
		default:
			rw.WriteHeader(404)
		}
	}))
	w.api = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/k-kanke/genkan/pulls":
			head := strings.TrimPrefix(r.URL.Query().Get("head"), "k-kanke:")
			if n, ok := w.prs[head]; ok {
				json.NewEncoder(rw).Encode([]map[string]any{{"number": n, "html_url": "https://github.com/k-kanke/genkan/pull/" + itoa(n)}})
				return
			}
			rw.Write([]byte("[]"))
		case "POST /repos/k-kanke/genkan/pulls":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			w.created = append(w.created, in)
			w.nextPR++
			w.prs[in["head"].(string)] = w.nextPR
			rw.WriteHeader(201)
			json.NewEncoder(rw).Encode(map[string]any{"number": w.nextPR, "html_url": "https://github.com/k-kanke/genkan/pull/" + itoa(w.nextPR)})
		default:
			rw.WriteHeader(404)
		}
	}))
	t.Cleanup(func() { w.cp.Close(); w.api.Close() })
	return w
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// patchFor builds a patch the way the Implementer does: edit a clone, `git add -A`, `git diff --cached --binary`.
func (w *world) patchFor(edit func(dir string)) []byte {
	w.t.Helper()
	dir := w.t.TempDir()
	git(w.t, dir, "clone", "-q", w.bare, ".")
	edit(dir)
	git(w.t, dir, "add", "-A")
	return []byte(git(w.t, dir, "diff", "--cached", "--binary", "--no-color"))
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (w *world) cfg(data []byte) Config {
	w.patchData = data
	return Config{Repo: "k-kanke/genkan", TaskID: "t1", PatchSHA: sha(data), Token: "ghs_secret", GitBase: w.base, APIBase: w.api.URL,
		PRTitle: "READMEに1行足す", PRBody: "body", ControlPlaneURL: w.cp.URL, PublisherToken: "pub-token"}
}

func (w *world) remoteFile(branch, path string) string {
	w.t.Helper()
	return git(w.t, w.bare, "show", branch+":"+path)
}

func TestPublishesApprovedPatchAsABranchAndARegularPR(t *testing.T) {
	w := newWorld(t)
	data := w.patchFor(func(d string) { os.WriteFile(filepath.Join(d, "README.md"), []byte("hello\nworld\n"), 0o644) })
	res, err := Run(context.Background(), w.cfg(data))
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "agent/t1" || res.PRNumber != 42 || !strings.HasSuffix(res.PRURL, "/pull/42") {
		t.Fatalf("result: %+v", res)
	}
	if got := w.remoteFile("agent/t1", "README.md"); got != "hello\nworld\n" {
		t.Errorf("branch content: %q", got)
	}
	if got := w.remoteFile("main", "README.md"); got != "hello\n" {
		t.Errorf("main must be untouched: %q", got)
	}
	if len(w.created) != 1 || w.created[0]["draft"] != false || w.created[0]["base"] != "main" || w.created[0]["head"] != "agent/t1" {
		t.Errorf("PR request: %v", w.created)
	}
	if len(w.published) != 1 || w.published[0].PRNumber != 42 {
		t.Errorf("the Control Plane must be told about the PR: %v", w.published)
	}
	if n := strings.TrimSpace(git(t, w.bare, "rev-list", "--count", "agent/t1")); n != "2" {
		t.Errorf("one commit on top of main, got %s commits", n)
	}
}

func TestRefusesAPatchThatIsNotWhatWasApproved(t *testing.T) {
	w := newWorld(t)
	data := w.patchFor(func(d string) { os.WriteFile(filepath.Join(d, "README.md"), []byte("changed\n"), 0o644) })
	cfg := w.cfg(data)
	cfg.PatchSHA = sha([]byte("something else"))
	if _, err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "approved hash") {
		t.Fatalf("hash mismatch must be refused: %v", err)
	}
	if out := git(t, w.bare, "branch", "--list", "agent/*"); strings.TrimSpace(out) != "" {
		t.Errorf("nothing may be pushed: %q", out)
	}
}

func TestRefusesRiskyChanges(t *testing.T) {
	cases := map[string]func(d string){
		"workflow": func(d string) {
			os.MkdirAll(filepath.Join(d, ".github/workflows"), 0o755)
			os.WriteFile(filepath.Join(d, ".github/workflows/x.yml"), []byte("on: push\n"), 0o644)
		},
		"lockfile":   func(d string) { os.WriteFile(filepath.Join(d, "package-lock.json"), []byte("{\"a\":1}\n"), 0o644) },
		"dockerfile": func(d string) { os.WriteFile(filepath.Join(d, "Dockerfile"), []byte("FROM scratch\n"), 0o644) },
		"symlink":    func(d string) { os.Symlink("/etc/passwd", filepath.Join(d, "link")) },
		"exec bit":   func(d string) { os.Chmod(filepath.Join(d, "README.md"), 0o755) },
		"binary":     func(d string) { os.WriteFile(filepath.Join(d, "x.bin"), []byte{0, 1, 2, 3, 0, 255}, 0o644) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			data := w.patchFor(edit)
			if _, err := Run(context.Background(), w.cfg(data)); err == nil || !strings.Contains(err.Error(), "publish refused") {
				t.Fatalf("%s must be refused, got %v", name, err)
			}
			if out := git(t, w.bare, "branch", "--list", "agent/*"); strings.TrimSpace(out) != "" {
				t.Errorf("nothing may be pushed: %q", out)
			}
			if len(w.created) != 0 {
				t.Errorf("no PR may be opened")
			}
		})
	}
}

func TestPatchThatNoLongerAppliesFails(t *testing.T) {
	w := newWorld(t)
	data := w.patchFor(func(d string) { os.WriteFile(filepath.Join(d, "README.md"), []byte("hello\nworld\n"), 0o644) })
	// main moves on after the patch was made.
	other := t.TempDir()
	git(t, other, "clone", "-q", w.bare, ".")
	os.WriteFile(filepath.Join(other, "README.md"), []byte("rewritten\n"), 0o644)
	git(t, other, "commit", "-qam", "conflict")
	git(t, other, "push", "-q", "origin", "main")
	if _, err := Run(context.Background(), w.cfg(data)); err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("want a clear 'does not apply', got %v", err)
	}
}

func TestRetryReusesItsOwnBranchAndPR(t *testing.T) {
	w := newWorld(t)
	data := w.patchFor(func(d string) { os.WriteFile(filepath.Join(d, "README.md"), []byte("hello\nworld\n"), 0o644) })
	cfg := w.cfg(data)
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg) // the Job is retried after everything already happened
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.PRNumber != 42 || len(w.created) != 1 {
		t.Errorf("the PR must be reused, not duplicated: %+v created=%d", res, len(w.created))
	}
}

func TestTokenNeverAppearsInErrors(t *testing.T) {
	w := newWorld(t)
	data := w.patchFor(func(d string) { os.WriteFile(filepath.Join(d, "README.md"), []byte("x\n"), 0o644) })
	cfg := w.cfg(data)
	cfg.GitBase = "file:///does/not/exist"
	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("clone of a missing repository must fail")
	}
	if strings.Contains(err.Error(), "ghs_secret") {
		t.Errorf("token leaked into the error: %v", err)
	}
}

func TestCheckAcceptsOrdinaryChanges(t *testing.T) {
	ok := patch.Summarize([]byte("diff --git a/src/a.ts b/src/a.ts\n--- a/src/a.ts\n+++ b/src/a.ts\n@@ -1 +1 @@\n-a\n+b\n"))
	if err := Check(ok); err != nil {
		t.Errorf("ordinary change refused: %v", err)
	}
	if err := Check(patch.Summary{}); err == nil {
		t.Error("an empty patch must be refused")
	}
}
