package prepare

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	root := t.TempDir()
	host := func(p string) string { return filepath.Join(root, p) }
	if err := os.MkdirAll(host("/srv/swarm"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Chowning to an arbitrary uid needs CAP_CHOWN (root); a real prep job
	// always runs as root in its container, but a CI runner's own user
	// typically doesn't have it - skip that one case there instead of
	// asserting a privilege the test environment doesn't have.
	owner := ""
	if os.Geteuid() == 0 {
		owner = "1000:1000"
	}

	// creates missing folders
	if err := Run(root, []Spec{{Prefix: "/srv/swarm", Path: "/srv/swarm/app/prod/uploads", Create: true}}, owner); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(host("/srv/swarm/app/prod/uploads"))
	if err != nil || !fi.IsDir() {
		t.Fatal("folder not created")
	}
	// idempotent
	if err := Run(root, []Spec{{Prefix: "/srv/swarm", Path: "/srv/swarm/app/prod/uploads", Create: true}}, ""); err != nil {
		t.Fatal(err)
	}

	// refuses symlinks anywhere in the path
	if err := os.Symlink("/etc", host("/srv/swarm/app/prod/evil")); err != nil {
		t.Fatal(err)
	}
	err = Run(root, []Spec{{Prefix: "/srv/swarm", Path: "/srv/swarm/app/prod/evil/x", Create: true}}, "")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
	if _, err := os.Stat("/etc/x"); err == nil && os.Getenv("CI") == "" {
		// not created through the symlink
		t.Log("note: /etc/x exists on this machine")
	}

	// check-only folders must exist
	err = Run(root, []Spec{{Prefix: "/srv/swarm", Path: "/srv/swarm/app/prod/missing"}}, "")
	if err == nil || !strings.Contains(err.Error(), "not created automatically") {
		t.Fatalf("got %v", err)
	}

	// missing base folder is reported clearly
	err = Run(root, []Spec{{Prefix: "/srv/other", Path: "/srv/other/x", Create: true}}, "")
	if err == nil || !strings.Contains(err.Error(), "an admin must create it") {
		t.Fatalf("got %v", err)
	}

	// file instead of folder
	_ = os.WriteFile(host("/srv/swarm/file"), []byte("x"), 0o600)
	if err := Run(root, []Spec{{Prefix: "/srv/swarm", Path: "/srv/swarm/file/x", Create: true}}, ""); err == nil {
		t.Fatal("expected error for file in path")
	}
	// but a non-directory leaf itself (e.g. a socket like
	// /var/run/docker.sock) is fine - Docker can bind-mount a file just as
	// well as a directory, only existence and "not a symlink" matter.
	if err := Run(root, []Spec{{Prefix: "/srv/swarm", Path: "/srv/swarm/file"}}, ""); err != nil {
		t.Fatalf("non-directory leaf should be accepted: %v", err)
	}
	// outside the prefix
	if err := Run(root, []Spec{{Prefix: "/srv/swarm", Path: "/etc/x", Create: true}}, ""); err == nil {
		t.Fatal("expected error outside prefix")
	}
}

func TestSpecRoundTrip(t *testing.T) {
	s := Spec{Prefix: "/srv/swarm", Path: "/srv/swarm/a b/c", Create: true}
	got, err := ParseSpec(s.String())
	if err != nil || got != s {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ParseSpec("delete|/|/"); err == nil {
		t.Fatal("expected error")
	}
}
