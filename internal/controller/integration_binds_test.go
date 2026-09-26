//go:build integration

package controller

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/andipunz/swarm-gitops/internal/config"
	"github.com/andipunz/swarm-gitops/internal/policy"
	"github.com/andipunz/swarm-gitops/internal/registry"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

const bindRoot = "/tmp/sg-it"

func webStack(extra string) string {
	return `services:
  web:
    image: localtest/web:1
` + extra
}

func simpleRepo(stack string) map[string]string {
	return map[string]string{"deploy.yml": "environments:\n  prod:\n    ref: main\n", "stack.yml": stack}
}

// TestBindsAndHosts: bind folders are created on the node, symlinks and
// formerly writable parents are refused, and hostnames belong to one stack.
func TestBindsAndHosts(t *testing.T) {
	if out, err := exec.Command("docker", "info", "--format", "{{.Swarm.LocalNodeState}}").Output(); err != nil || strings.TrimSpace(string(out)) != "active" {
		t.Skip("needs an active swarm")
	}
	if exec.Command("docker", "image", "inspect", "localtest/swarm-gitops:test").Run() != nil {
		t.Skip("run testdata/web/build.sh first")
	}
	stacks := []string{"files-prod", "site-a-prod", "site-b-prod", "legacy-web", "other-prod"}
	clean := func() {
		for _, s := range stacks {
			_ = exec.Command("docker", "stack", "rm", s).Run()
		}
		time.Sleep(3 * time.Second)
	}
	clean()
	defer clean()
	_ = os.RemoveAll(bindRoot)
	if err := os.MkdirAll(bindRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(bindRoot)

	pol := policy.Default()
	pol.BindMounts = []policy.BindRule{
		{Path: bindRoot + "/{repo}/{env}", Create: true},
		{Path: "/tmp/sg-missing/{repo}", Create: true},
	}
	pol.Traefik = policy.TraefikPolicy{Hosts: map[string][]string{"*": {"*.example.org"}}}

	dataDir := t.TempDir()
	cfg := &config.Config{
		RolloutTimeout: 60 * time.Second, PruneEnabled: true, PruneConfirm: 2, MaxPrune: 3, Concurrency: 1,
		DataDir: dataDir, PrepImage: "localtest/swarm-gitops:test", PrepTimeout: 60 * time.Second,
	}
	st, _ := state.Open(dataDir + "/state.json")
	git := newFakeGit()
	var logOut io.Writer = io.Discard
	if os.Getenv("VERBOSE") != "" {
		logOut = os.Stderr
	}
	c := New(cfg, git, &swarm.Docker{Env: os.Environ()}, registry.NewResolver(""), pol, st, slog.New(slog.NewTextHandler(logOut, nil)))
	ctx := context.Background()

	lastFor := func(repo string) check {
		git.mu.Lock()
		defer git.mu.Unlock()
		for i := len(git.checks) - 1; i >= 0; i-- {
			if git.checks[i].repo == repo {
				return git.checks[i]
			}
		}
		return check{}
	}
	expect := func(t *testing.T, repo, conclusion, contains string) {
		t.Helper()
		ch := lastFor(repo)
		if ch.conclusion != conclusion || !strings.Contains(ch.title+"\n"+ch.summary, contains) {
			t.Fatalf("want %s containing %q, got %s: %s\n%s", conclusion, contains, ch.conclusion, ch.title, ch.summary)
		}
	}

	t.Run("bind folder is created on the node and mounted", func(t *testing.T) {
		f := simpleRepo(webStack("    volumes: ['${STACK_DATA}/uploads:/uploads']\n"))
		f["deploy.yml"] = "environments:\n  prod:\n    ref: main\n    bind_owner: \"1000:1000\"\n"
		git.push("files", "main", f)
		c.Scan(ctx)
		expect(t, "files", "success", "Deployed")
		fi, err := os.Stat(bindRoot + "/files/prod/uploads")
		if err != nil || !fi.IsDir() {
			t.Fatalf("folder not created: %v", err)
		}
		if out, _ := exec.Command("stat", "-c", "%u:%g", bindRoot+"/files/prod/uploads").Output(); strings.TrimSpace(string(out)) != "1000:1000" {
			t.Fatalf("owner = %s", out)
		}
		if n := strings.TrimSpace(sh(t, "service", "ls", "-q", "--filter", "label=swarm-gitops.prep")); n != "" {
			t.Fatal("prepare job was not cleaned up")
		}
	})

	t.Run("bind outside the allowed folders is refused", func(t *testing.T) {
		git.push("files", "main", simpleRepo(webStack("    volumes: ['/etc:/host-etc']\n")))
		c.Scan(ctx)
		expect(t, "files", "failure", "use a folder below "+bindRoot+"/files/prod")
	})

	t.Run("symlink planted by a container is refused", func(t *testing.T) {
		// what a container with the folder mounted could do
		if err := os.Symlink("/etc", bindRoot+"/files/prod/uploads/evil"); err != nil {
			t.Fatal(err)
		}
		git.push("files", "main", simpleRepo(webStack("    volumes: ['${STACK_DATA}/uploads/evil:/x']\n")))
		c.Scan(ctx)
		// uploads was mounted writable before -> refused by the history rule already
		expect(t, "files", "failure", "was mounted writable before")

		// a symlink next to (not below) earlier mounts is caught on the node
		if err := os.Symlink("/etc", bindRoot+"/files/prod/sneaky"); err != nil {
			t.Fatal(err)
		}
		git.push("files", "main", simpleRepo(webStack("    volumes: ['${STACK_DATA}/sneaky:/x']\n")))
		c.Scan(ctx)
		expect(t, "files", "failure", "symlink")
		if strings.Contains(sh(t, "service", "inspect", "files-prod_web", "--format", "{{json .Spec.TaskTemplate.ContainerSpec.Mounts}}"), "sneaky") {
			t.Fatal("symlinked folder was mounted")
		}
	})

	t.Run("reset-binds clears the history", func(t *testing.T) {
		_ = os.Remove(bindRoot + "/files/prod/uploads/evil")
		if err := c.ResetBinds(bindRoot + "/files/prod"); err != nil {
			t.Fatal(err)
		}
		git.push("files", "main", simpleRepo(webStack("    volumes: ['${STACK_DATA}/uploads/sub:/x']\n")))
		c.Scan(ctx)
		expect(t, "files", "success", "Deployed")
	})

	t.Run("missing base folder on the node is reported", func(t *testing.T) {
		git.push("other", "main", simpleRepo(webStack("    volumes: ['/tmp/sg-missing/other/data:/data']\n")))
		c.Scan(ctx)
		expect(t, "other", "failure", "an admin must create it once")
	})

	route := func(stack, host string) string {
		return webStack("    deploy:\n      labels:\n        traefik.enable: \"true\"\n        traefik.http.routers.${STACK}.rule: Host(`" + host + "`)\n        traefik.http.services.${STACK}.loadbalancer.server.port: \"8080\"\n")
	}

	t.Run("hostname belongs to the first stack", func(t *testing.T) {
		git.push("site-a", "main", simpleRepo(route("site-a", "www.example.org")))
		c.Scan(ctx)
		expect(t, "site-a", "success", "Deployed")
		git.push("site-b", "main", simpleRepo(route("site-b", "www.example.org")))
		c.Scan(ctx)
		expect(t, "site-b", "failure", "already used by stack site-a-prod")
		// site-a can keep redeploying its own host
		git.push("site-a", "main", nil)
		c.Scan(ctx)
		expect(t, "site-a", "success", "Deployed")
	})

	t.Run("hostnames of unmanaged stacks are respected", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(dir+"/s.yml", []byte(`services:
  web:
    image: localtest/web:1
    deploy:
      labels:
        traefik.http.routers.legacy.rule: "Host(`+"`legacy.example.org`"+`) || Host(`+"`old.example.org`"+`)"
`), 0o600)
		sh(t, "stack", "deploy", "-c", dir+"/s.yml", "--detach=true", "legacy-web")
		git.push("site-b", "main", simpleRepo(route("site-b", "old.example.org")))
		c.Scan(ctx)
		expect(t, "site-b", "failure", "already used by stack legacy-web")
	})

	t.Run("host outside the allowed domains", func(t *testing.T) {
		git.push("site-b", "main", simpleRepo(route("site-b", "www.google.com")))
		c.Scan(ctx)
		expect(t, "site-b", "failure", "not allowed in prod")
	})
}
