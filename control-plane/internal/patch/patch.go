// Package patch reads a `git diff --binary` produced by an Implementer and describes it.
//
// The patch comes from an untrusted Pod. Nothing here executes it: it only parses the text to
// summarise what changed (for the approval screen) and to flag risky changes. The Publisher applies
// the patch to a fresh clone later (docs/multi-agent-permissions.md §5.3) and runs the same checks.
package patch

import (
	"path"
	"sort"
	"strings"
)

// File is one changed file.
type File struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
	Symlink bool   `json:"symlink,omitempty"`    // created or changed as a symbolic link (mode 120000)
	ModeSet bool   `json:"modeChange,omitempty"` // permission bits changed
	Risk    string `json:"risk,omitempty"`       // why this file needs a closer look ("" = ordinary)
}

// Summary describes a whole patch.
type Summary struct {
	Files   []File   `json:"files"`
	Added   int      `json:"added"`
	Deleted int      `json:"deleted"`
	Bytes   int      `json:"bytes"`
	Risks   []string `json:"risks,omitempty"` // human-readable, one per risky file
	// Blocked is true when the patch must not be published at all (not merely flagged).
	Blocked bool `json:"blocked,omitempty"`
}

var lockfiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "npm-shrinkwrap.json": true,
	"go.sum": true, "Cargo.lock": true, "poetry.lock": true, "Pipfile.lock": true, "Gemfile.lock": true, "composer.lock": true,
}

// Risk explains why p needs attention; "" means an ordinary file. blocked is true for paths that
// can never be published (they could change what git itself runs, or escape the repository).
func Risk(p string) (reason string, blocked bool) {
	clean := path.Clean(p)
	switch {
	case p == "" || strings.HasPrefix(p, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(p, "\x00"):
		return "リポジトリの外を指すパス", true
	case clean == ".git" || strings.HasPrefix(clean, ".git/"):
		return ".git の中(git の動作を書き換えられる)", true
	}
	base := path.Base(clean)
	switch {
	case clean == ".github" || strings.HasPrefix(clean, ".github/"):
		return "CI・権限の定義(.github)", false
	case base == "CODEOWNERS":
		return "レビュー担当の定義(CODEOWNERS)", false
	case clean == ".gitmodules" || clean == ".gitattributes":
		return "git の設定(" + base + ")", false
	case lockfiles[base]:
		return "依存の固定ファイル(" + base + ")", false
	case strings.HasPrefix(base, "Dockerfile") || strings.HasSuffix(base, ".dockerfile"):
		return "コンテナの定義(" + base + ")", false
	case clean == "deploy" || strings.HasPrefix(clean, "deploy/") || clean == "k8s" || strings.HasPrefix(clean, "k8s/"):
		return "デプロイの定義(" + strings.SplitN(clean, "/", 2)[0] + ")", false
	}
	return "", false
}

// Summarize parses a unified git diff (with --binary). Unknown lines are ignored; it never fails,
// because the point is to show the human what is there, not to reject on a parsing quirk.
func Summarize(diff []byte) Summary {
	s := Summary{Bytes: len(diff)}
	var cur *File
	inHunk := false
	flush := func() {
		if cur == nil {
			return
		}
		if cur.Risk == "" {
			if r, b := Risk(cur.Path); r != "" {
				cur.Risk = r
				if b {
					s.Blocked = true
				}
			}
		}
		if cur.Symlink && cur.Risk == "" {
			cur.Risk = "シンボリックリンク"
		}
		if cur.Risk != "" {
			s.Risks = append(s.Risks, cur.Path+": "+cur.Risk)
		}
		s.Added += cur.Added
		s.Deleted += cur.Deleted
		s.Files = append(s.Files, *cur)
		cur = nil
	}
	for _, line := range strings.Split(string(diff), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			inHunk = false
			cur = &File{Path: headerPath(strings.TrimPrefix(line, "diff --git "))}
		case cur == nil:
			// preamble before the first file
		case !inHunk && strings.HasPrefix(line, "rename to "):
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case !inHunk && (strings.HasPrefix(line, "new file mode 120000") || strings.HasPrefix(line, "new mode 120000")):
			cur.Symlink = true
		case !inHunk && (strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ")):
			cur.ModeSet = true
		case !inHunk && (strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ")):
			cur.Binary = true
		case !inHunk && strings.HasPrefix(line, "+++ b/"):
			cur.Path = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(line, "+"):
			cur.Added++
		case inHunk && strings.HasPrefix(line, "-"):
			cur.Deleted++
		}
	}
	flush()
	sort.SliceStable(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
	return s
}

// headerPath extracts the path from "a/<p> b/<p>". For a rename the two differ; the
// "rename to" line then overrides this.
func headerPath(rest string) string {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "a/") {
		return rest
	}
	rest = strings.TrimPrefix(rest, "a/")
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return rest
}
