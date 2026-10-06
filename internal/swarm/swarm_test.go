package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDocker puts a `docker` script first on PATH. `service ls -q` lists
// lsOut; each `service inspect` call is answered by the next entry of
// inspects, where "gone" fails like a service removed in between.
func fakeDocker(t *testing.T, lsOut string, inspects ...string) (calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	for i, out := range inspects {
		if err := os.WriteFile(filepath.Join(dir, "inspect"+string(rune('0'+i))), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$2" in
ls) printf '%s\n' ` + lsOut + ` ;;
inspect)
  n=$(grep -c '^service inspect' "` + log + `")
  f="` + dir + `/inspect$((n-1))"
  if [ "$(cat "$f")" = gone ]; then echo "Error: no such service: x2" >&2; exit 1; fi
  cat "$f" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func TestListAndInspectRetriesVanishedService(t *testing.T) {
	calls := fakeDocker(t, "x1 x2", "gone", `[{"ID":"x1","Spec":{"Name":"web_app"}}]`)
	d := &Docker{Env: os.Environ()}
	svcs, err := d.AllServices(context.Background())
	if err != nil {
		t.Fatalf("a service vanishing between ls and inspect must not fail the call: %v", err)
	}
	if len(svcs) != 1 || svcs[0].Spec.Name != "web_app" {
		t.Fatalf("svcs = %+v", svcs)
	}
	if got := calls(); len(got) != 4 { // ls, inspect (fails), ls, inspect
		t.Fatalf("docker calls = %q, want ls+inspect twice", got)
	}
}

func TestListAndInspectGivesUp(t *testing.T) {
	fakeDocker(t, "x1", "gone", "gone", "gone", "gone")
	d := &Docker{Env: os.Environ()}
	if _, err := d.Services(context.Background(), "a=b"); err == nil || !strings.Contains(err.Error(), "no such service") {
		t.Fatalf("err = %v, want the no such service error after %d attempts", err, inspectAttempts)
	}
}
