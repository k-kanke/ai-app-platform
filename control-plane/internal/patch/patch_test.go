package patch

import "testing"

const sample = `diff --git a/src/pages/Infra.tsx b/src/pages/Infra.tsx
index 111..222 100644
--- a/src/pages/Infra.tsx
+++ b/src/pages/Infra.tsx
@@ -1,3 +1,4 @@
 keep
-old
+new
+more
diff --git a/.github/workflows/build.yml b/.github/workflows/build.yml
index 1..2 100644
--- a/.github/workflows/build.yml
+++ b/.github/workflows/build.yml
@@ -1 +1 @@
-a
+b
diff --git a/package-lock.json b/package-lock.json
--- a/package-lock.json
+++ b/package-lock.json
@@ -1 +1,2 @@
 x
+y
diff --git a/logo.png b/logo.png
new file mode 100644
index 0..1
GIT binary patch
literal 10
Lc$@
diff --git a/link b/link
new file mode 120000
index 0..1
--- /dev/null
+++ b/link
@@ -0,0 +1 @@
+/etc/passwd
\ No newline at end of file
diff --git a/old.txt b/new.txt
similarity index 100%
rename from old.txt
rename to new.txt
`

func TestSummarize(t *testing.T) {
	s := Summarize([]byte(sample))
	if len(s.Files) != 6 {
		t.Fatalf("files = %d: %+v", len(s.Files), s.Files)
	}
	by := map[string]File{}
	for _, f := range s.Files {
		by[f.Path] = f
	}
	if f := by["src/pages/Infra.tsx"]; f.Added != 2 || f.Deleted != 1 || f.Risk != "" {
		t.Errorf("Infra.tsx = %+v", f)
	}
	if f := by[".github/workflows/build.yml"]; f.Risk == "" {
		t.Errorf("workflow should be flagged: %+v", f)
	}
	if f := by["package-lock.json"]; f.Risk == "" || f.Added != 1 {
		t.Errorf("lockfile = %+v", f)
	}
	if f := by["logo.png"]; !f.Binary {
		t.Errorf("binary not detected: %+v", f)
	}
	if f := by["link"]; !f.Symlink || f.Risk == "" {
		t.Errorf("symlink not flagged: %+v", f)
	}
	if _, ok := by["new.txt"]; !ok {
		t.Errorf("rename target missing: %+v", s.Files)
	}
	if s.Added != 5 || s.Deleted != 2 {
		t.Errorf("totals = +%d -%d", s.Added, s.Deleted)
	}
	if s.Blocked {
		t.Error("nothing here is blocked")
	}
	if len(s.Risks) != 3 {
		t.Errorf("risks = %v", s.Risks)
	}
}

func TestBlockedPaths(t *testing.T) {
	for _, p := range []string{".git/hooks/pre-commit", ".git/config", "../etc/passwd", "/etc/passwd", "a/../../x"} {
		if _, blocked := Risk(p); !blocked {
			t.Errorf("%q must be blocked", p)
		}
	}
	for _, p := range []string{"src/a.ts", ".gitignore", "docs/.github-note.md", "Dockerfile.dev"} {
		if _, blocked := Risk(p); blocked {
			t.Errorf("%q must not be blocked", p)
		}
	}
	d := "diff --git a/.git/hooks/post-checkout b/.git/hooks/post-checkout\nnew file mode 100755\n--- /dev/null\n+++ b/.git/hooks/post-checkout\n@@ -0,0 +1 @@\n+evil\n"
	if s := Summarize([]byte(d)); !s.Blocked || len(s.Risks) != 1 {
		t.Errorf("hook patch: %+v", s)
	}
}

func TestEmptyPatch(t *testing.T) {
	if s := Summarize(nil); len(s.Files) != 0 || s.Blocked {
		t.Errorf("%+v", s)
	}
}
