package kube

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs the release-strategy helper scripts for real, in the same busybox image the cluster uses.
// Needs Docker, so it only runs with AAP_DOCKER_TESTS=1:
//
//	AAP_DOCKER_TESTS=1 go test ./internal/kube -run TestReleaseScriptsInBusybox -v
func TestReleaseScriptsInBusybox(t *testing.T) {
	if os.Getenv("AAP_DOCKER_TESTS") != "1" {
		t.Skip("set AAP_DOCKER_TESTS=1 to run the scripts in busybox")
	}
	home, _ := os.UserHomeDir() // Docker Desktop shares $HOME; /var/folders is not shared
	work, err := os.MkdirTemp(home, ".aap-script-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	for _, d := range []string{"src", "data", "pdata", "dsnap", "scripts"} {
		os.MkdirAll(filepath.Join(work, d), 0o777)
	}
	write := func(name string, role string, n int) {
		s, err := releaseScript(role, n)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(work, "scripts", name), []byte(s), 0o755)
	}
	write("rinit", RoleRInit, 0)
	write("dcopy", RoleDataCopy, 0)
	write("rel1", RoleRelease, 1)
	write("rel2", RoleRelease, 2)
	write("dsnap2", RoleDSnap, 2)
	write("drestore2", RoleDRestore, 2)
	write("reset1", RoleReset, 1)

	// One container, one long script: the scripts must compose, as they do on the cluster's PVCs.
	driver := `
set -e
run() { echo ">> $1"; sh /scripts/$1; }
run rinit
echo v1 > /src/draft/index.html; echo '{"a":1}' > /data/meals.json
run rel1 | tee /tmp/rel1.out
h1=$(sed -n 's/^AAP_HASH //p' /tmp/rel1.out)
test "$(cat /src/releases/1.sha256)" = "$h1"
# release 1 is read-only even for the file's owner
if echo hack >> /src/releases/1/index.html 2>/dev/null && [ "$(id -u)" != "0" ]; then echo "NOT READ ONLY"; exit 1; fi
test "$(stat -c %a /src/releases/1/index.html)" = "444" || test "$(stat -c %a /src/releases/1/index.html)" = "555"
# the draft keeps changing, release 1 does not
echo v2 > /src/draft/index.html
test "$(cat /src/releases/1/index.html)" = "v1"
run rel2 | tee /tmp/rel2.out
h2=$(sed -n 's/^AAP_HASH //p' /tmp/rel2.out)
test "$h1" != "$h2"; echo "hashes differ: $h1 / $h2"
# running the same release again from the same draft gives the same hash (idempotent retry)
run rel2 | sed -n 's/^AAP_HASH //p' > /tmp/rel2b.out
test "$(cat /tmp/rel2b.out)" = "$h2"
# data: snapshot, change, restore
run dsnap2
echo '{"a":2}' > /data/meals.json
run drestore2
test "$(cat /data/meals.json)" = '{"a":1}'
# preview data is a copy; writing to it never changes production data
run dcopy
echo '{"preview":true}' > /pdata/meals.json
test "$(cat /data/meals.json)" = '{"a":1}'
# discard: the draft goes back to release 1 and is writable again
run reset1
test "$(cat /src/draft/index.html)" = "v1"
echo ok >> /src/draft/index.html
echo SCRIPTS-OK
`
	cmd := exec.Command("docker", "run", "--rm",
		"-v", filepath.Join(work, "src")+":/src", "-v", filepath.Join(work, "data")+":/data",
		"-v", filepath.Join(work, "pdata")+":/pdata", "-v", filepath.Join(work, "dsnap")+":/dsnap",
		"-v", filepath.Join(work, "scripts")+":/scripts:ro", "busybox:1.37", "sh", "-c", driver)
	out, err := cmd.CombinedOutput()
	t.Log("\n" + string(out))
	if err != nil || !strings.Contains(string(out), "SCRIPTS-OK") {
		t.Fatalf("scripts failed: %v", err)
	}
}
