//go:build integration

// End-to-end test against a real (single node) Swarm with a fake GitHub.
// Needs: docker swarm init, and images localtest/web:1 and localtest/web:2.
//
//	go test -tags integration -v ./internal/controller/
package controller

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andipunz/swarm-gitops/internal/config"
	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/policy"
	"github.com/andipunz/swarm-gitops/internal/registry"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

// ---- fake GitHub -----------------------------------------------------------

type fakeBranch struct {
	commit, tree, parentTree string
	files                    map[string]string // relative to .swarm/
}

type fakeRepo struct {
	archived bool
	branches map[string]*fakeBranch
}

type check struct{ repo, sha, name, conclusion, title, summary, text string }

type fakeGit struct {
	mu        sync.Mutex
	repos     map[string]*fakeRepo
	trees     map[string]map[string]string
	blobs     map[string]string
	checks    []check
	open      map[int64]*check
	depStates []string
	n         int
	allowed   map[string][]string
	protected bool
}

func newFakeGit() *fakeGit {
	return &fakeGit{repos: map[string]*fakeRepo{}, trees: map[string]map[string]string{}, blobs: map[string]string{}, open: map[int64]*check{}, protected: true}
}

func hash(s string) string { h := sha1.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

// push sets the files of a branch; nil files = empty commit (same tree).
func (f *fakeGit) push(repo, branch string, files map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	if r == nil {
		r = &fakeRepo{branches: map[string]*fakeBranch{}}
		f.repos[repo] = r
	}
	prev := r.branches[branch]
	if files == nil {
		files = prev.files
	}
	var keys []string
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "\x00" + files[k] + "\x00")
		f.blobs[hash(files[k])] = files[k]
	}
	tree := hash(b.String())
	f.trees[tree] = files
	f.n++
	nb := &fakeBranch{commit: hash(fmt.Sprint(tree, f.n)), tree: tree, files: files}
	if prev != nil {
		nb.parentTree = prev.tree
	}
	r.branches[branch] = nb
	return nb.commit
}

func (f *fakeGit) ListRepos() ([]gh.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res []gh.Repo
	for name, r := range f.repos {
		repo := gh.Repo{Name: name, Archived: r.archived, DefaultBranch: "main"}
		if m := r.branches["main"]; m != nil {
			repo.DefaultHead = m.commit
			if d, ok := m.files["deploy.yml"]; ok {
				repo.DeployFile = &d
			}
		}
		res = append(res, repo)
	}
	return res, nil
}

func (f *fakeGit) Branches(repo string) ([]gh.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res []gh.Branch
	for name, b := range f.repos[repo].branches {
		br := gh.Branch{Name: name, Commit: b.commit, TreeOid: b.tree, ParentTree: b.parentTree}
		if len(b.files) > 0 {
			br.SwarmTree = b.tree
		}
		res = append(res, br)
	}
	return res, nil
}

func (f *fakeGit) EnvProperties(string) (map[string][]string, error) { return f.allowed, nil }

func (f *fakeGit) Tree(_ string, oid string) ([]gh.TreeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res []gh.TreeEntry
	for p, c := range f.trees[oid] {
		res = append(res, gh.TreeEntry{Path: p, Mode: "100644", Type: "blob", Sha: hash(c), Size: int64(len(c))})
	}
	return res, nil
}

func (f *fakeGit) Blob(_ string, sha string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.blobs[sha]), nil
}

func (f *fakeGit) BranchProtected(string, string) (bool, error) { return f.protected, nil }

// CommitCheckRuns reports no CI; the tests run with CI_WAIT_TIMEOUT=0 anyway.
func (f *fakeGit) CommitCheckRuns(string, string) ([]gh.CheckRunStatus, error) { return nil, nil }

func (f *fakeGit) CreateCheckRun(repo, sha, name string) (*gh.CheckRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.open[int64(f.n)] = &check{repo: repo, sha: sha, name: name}
	return &gh.CheckRun{ID: int64(f.n), HTMLURL: "https://example/check"}, nil
}

func (f *fakeGit) CompleteCheckRun(_ string, id int64, conclusion, title, summary, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.open[id]
	c.conclusion, c.title, c.summary, c.text = conclusion, title, summary, text
	f.checks = append(f.checks, *c)
	delete(f.open, id)
	return nil
}

func (f *fakeGit) CreateDeployment(repo, sha, env, desc string, _, _ bool) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return int64(f.n), nil
}

func (f *fakeGit) DeploymentStatus(_ string, _ int64, state, desc, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depStates = append(f.depStates, state+": "+desc)
	return nil
}

func (f *fakeGit) MarkEnvironmentInactive(repo, env, desc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depStates = append(f.depStates, "inactive: "+env+" "+desc)
	return nil
}

// lastCheck returns the newest completed check run with that name.
func (f *fakeGit) lastCheck(name string) check {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.checks) - 1; i >= 0; i-- {
		if f.checks[i].name == name {
			return f.checks[i]
		}
	}
	return check{}
}

func (f *fakeGit) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.checks)
}

// ---- helpers ---------------------------------------------------------------

const deploy = `environments:
  prod:
    ref: main
    files: [stack.yml, stack.prod.yml]
    url: https://web.example.org
  staging:
    ref: develop
`

func stackYML(image string, replicas int) string {
	return fmt.Sprintf(`services:
  web:
    image: %s
    environment:
      ENVIRONMENT: ${ENV}
    configs:
      - source: app
        target: /app.conf
    deploy:
      replicas: %d
      update_config:
        failure_action: rollback
        monitor: 5s
configs:
  app:
    file: ./app.conf
`, image, replicas)
}

func files(image string, replicas int, conf string) map[string]string {
	return map[string]string{
		"deploy.yml":     deploy,
		"stack.yml":      stackYML(image, replicas),
		"stack.prod.yml": "services:\n  web:\n    deploy:\n      labels:\n        tier: prod\n",
		"app.conf":       conf,
	}
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func cleanup() {
	for _, s := range []string{"web-prod", "web-staging", "legacy-prod"} {
		_ = exec.Command("docker", "stack", "rm", s).Run()
	}
	time.Sleep(3 * time.Second)
}

func TestEndToEnd(t *testing.T) {
	if out, err := exec.Command("docker", "info", "--format", "{{.Swarm.LocalNodeState}}").Output(); err != nil || strings.TrimSpace(string(out)) != "active" {
		t.Skip("needs an active swarm")
	}
	cleanup()
	defer cleanup()

	dataDir := t.TempDir()
	cfg := &config.Config{
		EnvProperty: "swarm-environments", RequireProtected: []string{"prod"},
		RolloutTimeout: 90 * time.Second, PruneEnabled: true, PruneConfirm: 2, MaxPrune: 3, Concurrency: 2, DataDir: dataDir,
	}
	st, _ := state.Open(dataDir + "/state.json")
	git := newFakeGit()
	git.allowed = map[string][]string{"web": {"prod", "staging"}}
	var logOut io.Writer = io.Discard
	if os.Getenv("VERBOSE") != "" {
		logOut = os.Stderr
	}
	c := New(cfg, git, &swarm.Docker{Env: os.Environ()}, registry.NewResolver(""), policy.Default(), st,
		slog.New(slog.NewTextHandler(logOut, nil)))
	ctx := context.Background()

	stackLabel := func(stack, label string) string {
		return sh(t, "service", "inspect", stack+"_web", "--format", "{{index .Spec.Labels \""+label+"\"}}")
	}

	t.Run("initial deploy of prod and staging", func(t *testing.T) {
		git.push("web", "main", files("localtest/web:1", 2, "v1"))
		git.push("web", "develop", files("localtest/web:1", 1, "v1"))
		c.Scan(ctx)
		for _, env := range []string{"prod", "staging"} {
			if ch := git.lastCheck("swarm / " + env); ch.conclusion != "success" || ch.title != "Deployed" {
				t.Fatalf("%s: %+v", env, ch)
			}
		}
		if r := sh(t, "service", "ls", "--filter", "name=web-prod_web", "--format", "{{.Replicas}}"); r != "2/2" {
			t.Fatalf("prod replicas %s", r)
		}
		if stackLabel("web-prod", "tier") != "prod" {
			t.Fatal("prod override not applied")
		}
		if stackLabel("web-prod", "swarm-gitops.repo") != "web" {
			t.Fatal("controller labels missing")
		}
	})

	t.Run("metrics endpoint reports replica counts", func(t *testing.T) {
		m, err := c.Metrics(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, s := range m.Stacks {
			if s.Stack == "web-prod" && s.Service == "web-prod_web" {
				found = true
				if s.Desired != 2 || s.Running != 2 {
					t.Fatalf("web-prod_web replicas = %+v, want desired=2 running=2", s)
				}
			}
		}
		if !found {
			t.Fatalf("web-prod_web missing from metrics: %+v", m.Stacks)
		}
		if m.LastScan.IsZero() {
			t.Fatal("LastScan not set")
		}
	})

	t.Run("same commit is not processed twice", func(t *testing.T) {
		n := git.checkCount()
		c.Scan(ctx)
		if git.checkCount() != n {
			t.Fatal("unchanged commit produced new check runs")
		}
	})

	t.Run("commit without stack changes does not redeploy", func(t *testing.T) {
		before := sh(t, "service", "inspect", "web-prod_web", "--format", "{{.Version.Index}}")
		f := files("localtest/web:1", 2, "v1")
		f["README.md"] = "docs only"
		git.push("web", "main", f)
		c.Scan(ctx)
		if ch := git.lastCheck("swarm / prod"); ch.title != "No changes" {
			t.Fatalf("%+v", ch)
		}
		if after := sh(t, "service", "inspect", "web-prod_web", "--format", "{{.Version.Index}}"); after != before {
			t.Fatal("service was updated although nothing changed")
		}
	})

	t.Run("empty commit forces a redeploy", func(t *testing.T) {
		before := sh(t, "service", "inspect", "web-prod_web", "--format", "{{.Spec.TaskTemplate.ForceUpdate}}")
		git.push("web", "main", nil)
		c.Scan(ctx)
		after := sh(t, "service", "inspect", "web-prod_web", "--format", "{{.Spec.TaskTemplate.ForceUpdate}}")
		if after == before {
			t.Fatal("empty commit did not force an update")
		}
		if ch := git.lastCheck("swarm / prod"); ch.conclusion != "success" {
			t.Fatalf("%+v", ch)
		}
	})

	t.Run("config change rolls the service with a new config", func(t *testing.T) {
		git.push("web", "main", files("localtest/web:2", 3, "v2"))
		c.Scan(ctx)
		if ch := git.lastCheck("swarm / prod"); ch.conclusion != "success" {
			t.Fatalf("%+v", ch)
		}
		if r := sh(t, "service", "ls", "--filter", "name=web-prod_web", "--format", "{{.Replicas}} {{.Image}}"); r != "3/3 localtest/web:2" {
			t.Fatalf("got %q", r)
		}
		cfgs := sh(t, "config", "ls", "--filter", "label=com.docker.stack.namespace=web-prod", "--format", "{{.Name}}")
		if n := len(strings.Fields(cfgs)); n < 1 || n > 2 {
			t.Fatalf("old configs not pruned or missing: %q", cfgs)
		}
	})

	t.Run("policy violation is reported and not deployed", func(t *testing.T) {
		f := files("localtest/web:2", 3, "v2")
		f["stack.yml"] = strings.Replace(f["stack.yml"], "    environment:", "    volumes: ['/var/run/docker.sock:/var/run/docker.sock']\n    environment:", 1)
		git.push("web", "main", f)
		c.Scan(ctx)
		ch := git.lastCheck("swarm / prod")
		if ch.conclusion != "failure" || !strings.Contains(ch.summary, "bind mount") {
			t.Fatalf("%+v", ch)
		}
		if strings.Contains(sh(t, "service", "inspect", "web-prod_web", "--format", "{{json .Spec.TaskTemplate.ContainerSpec.Mounts}}"), "docker.sock") {
			t.Fatal("violating stack was deployed")
		}
	})

	t.Run("failed rollout is reported with rollback", func(t *testing.T) {
		git.push("web", "main", files("localtest/does-not-exist:1", 3, "v2"))
		c.Scan(ctx)
		ch := git.lastCheck("swarm / prod")
		if ch.conclusion != "failure" {
			t.Fatalf("%+v", ch)
		}
		t.Logf("failure report:\n%s\n%s", ch.summary, ch.text)
		// fix it again
		git.push("web", "main", files("localtest/web:2", 3, "v2"))
		c.Scan(ctx)
		if ch := git.lastCheck("swarm / prod"); ch.conclusion != "success" {
			t.Fatalf("%+v", ch)
		}
	})

	t.Run("unprotected branch blocks prod", func(t *testing.T) {
		git.protected = false
		defer func() { git.protected = true }()
		git.push("web", "main", nil)
		c.Scan(ctx)
		if ch := git.lastCheck("swarm / prod"); ch.title != "Branch is not protected" {
			t.Fatalf("%+v", ch)
		}
	})

	t.Run("unmanaged stack with the same name is not overwritten", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(dir+"/s.yml", []byte("services:\n  web:\n    image: localtest/web:1\n"), 0o600)
		sh(t, "stack", "deploy", "-c", dir+"/s.yml", "--detach=true", "legacy-prod")
		git.push("legacy", "main", map[string]string{
			"deploy.yml": "environments:\n  prod:\n    ref: main\n",
			"stack.yml":  stackYML("localtest/web:1", 1),
			"app.conf":   "legacy",
		})
		git.allowed["legacy"] = []string{"prod"}
		c.Scan(ctx)
		if ch := git.lastCheck("swarm / prod"); ch.repo != "legacy" || ch.title != "Stack name is taken" {
			t.Fatalf("%+v", ch)
		}
		// adopt -> takes over
		if err := c.Adopt("legacy-prod"); err != nil {
			t.Fatal(err)
		}
		c.Scan(ctx)
		if ch := git.lastCheck("swarm / prod"); ch.repo != "legacy" || ch.conclusion != "success" {
			t.Fatalf("%+v", ch)
		}
	})

	t.Run("environment removed: stack removed after 2 scans", func(t *testing.T) {
		f := files("localtest/web:2", 3, "v2")
		f["deploy.yml"] = "environments:\n  prod:\n    ref: main\n    files: [stack.yml, stack.prod.yml]\n"
		git.push("web", "main", f)
		c.Scan(ctx)
		if sh(t, "service", "ls", "-q", "--filter", "name=web-staging_web") == "" {
			t.Fatal("removed after the first scan, confirmation missing")
		}
		c.Scan(ctx)
		time.Sleep(2 * time.Second)
		if sh(t, "service", "ls", "-q", "--filter", "name=web-staging_web") != "" {
			t.Fatal("staging not removed after the second scan")
		}
	})

	t.Run("mass removal needs approval", func(t *testing.T) {
		cfg.MaxPrune = 1
		defer func() { cfg.MaxPrune = 3 }()
		git.repos["web"].archived = true
		git.repos["legacy"].archived = true
		c.Scan(ctx)
		c.Scan(ctx)
		var blocked []string
		st.View(func(d *state.Data) { blocked = d.PruneBlocked })
		if len(blocked) != 2 || sh(t, "service", "ls", "-q", "--filter", "name=web-prod_web") == "" {
			t.Fatalf("expected blocked removal of 2 stacks, got %v", blocked)
		}
		_ = c.ApprovePrune()
		c.Scan(ctx)
		time.Sleep(2 * time.Second)
		if sh(t, "service", "ls", "-q", "--filter", "label=swarm-gitops.managed=true") != "" {
			t.Fatal("archived repos' stacks not removed after approval")
		}
	})
}
