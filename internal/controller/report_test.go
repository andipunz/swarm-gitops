package controller

import (
	"log/slog"
	"os"
	"testing"

	"github.com/andipunz/swarm-gitops/internal/config"
	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/policy"
	"github.com/andipunz/swarm-gitops/internal/registry"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

// stubGit is a minimal Git implementation for report_test.go - the fuller
// fakeGit in integration_test.go lives behind the `integration` build tag,
// so it isn't available to a plain `go test ./...` run.
type stubGit struct {
	checkRunCreated   bool
	checkRunCompleted bool
}

func (s *stubGit) ListRepos() ([]gh.Repo, error)                      { return nil, nil }
func (s *stubGit) Branches(string) ([]gh.Branch, error)               { return nil, nil }
func (s *stubGit) EnvProperties(string) (map[string][]string, error)  { return nil, nil }
func (s *stubGit) Tree(string, string) ([]gh.TreeEntry, error)        { return nil, nil }
func (s *stubGit) Blob(string, string) ([]byte, error)                { return nil, nil }
func (s *stubGit) BranchProtected(string, string) (bool, error)       { return true, nil }
func (s *stubGit) MarkEnvironmentInactive(string, string, string) error { return nil }

func (s *stubGit) CreateCheckRun(repo, sha, name string) (*gh.CheckRun, error) {
	s.checkRunCreated = true
	return &gh.CheckRun{ID: 1, HTMLURL: "https://example/check/1"}, nil
}

func (s *stubGit) CompleteCheckRun(repo string, id int64, conclusion, title, summary, text string) error {
	s.checkRunCompleted = true
	return nil
}

func (s *stubGit) CreateDeployment(repo, sha, env, desc string, production, transient bool) (int64, error) {
	return 0, nil
}

func (s *stubGit) DeploymentStatus(repo string, id int64, state, desc, logURL, envURL string) error {
	return nil
}

func newTestController(t *testing.T, cfg *config.Config, git Git) *Controller {
	t.Helper()
	st, err := state.Open(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, git, &swarm.Docker{Env: os.Environ()}, registry.NewResolver(""), policy.Default(), st,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

func TestNewRunSkipsCheckRunUnderTokenAuth(t *testing.T) {
	target := Target{Repo: "example", Env: "prod", GitHubEnv: "prod", Branch: gh.Branch{Name: "main", Commit: "abc123"}}

	t.Run("token auth: no check run created", func(t *testing.T) {
		git := &stubGit{}
		c := newTestController(t, &config.Config{Token: "ghp_x"}, git)
		r := newRun(c, target, c.log)
		if r.check != nil {
			t.Fatal("expected no check run under token auth")
		}
		if git.checkRunCreated {
			t.Fatal("CreateCheckRun should not have been called under token auth")
		}
	})

	t.Run("GitHub App auth: check run still created", func(t *testing.T) {
		git := &stubGit{}
		c := newTestController(t, &config.Config{AppID: 1, AppKeyFile: "/key.pem"}, git)
		r := newRun(c, target, c.log)
		if r.check == nil {
			t.Fatal("expected a check run under GitHub App auth")
		}
		if !git.checkRunCreated {
			t.Fatal("CreateCheckRun should have been called under GitHub App auth")
		}
	})
}

// Regression test: under token auth (no check run to carry the summary),
// done() used to log only "result"/"title" - a policy violation's actual
// detail (in summary) was silently dropped everywhere, since the Checks API
// call that would have carried it always 403s for a PAT.
func TestRunDoneLogsSummaryWithoutCheckRun(t *testing.T) {
	git := &stubGit{}
	c := newTestController(t, &config.Config{Token: "ghp_x"}, git)
	target := Target{Repo: "example", Env: "prod", GitHubEnv: "prod", Branch: gh.Branch{Name: "main", Commit: "abc123"}}
	r := newRun(c, target, c.log)

	r.done("failure", "1 policy violation(s)", "some violation detail", "")

	if git.checkRunCompleted {
		t.Fatal("CompleteCheckRun should not have been called - there is no check run")
	}
	if !r.finished {
		t.Fatal("run should be marked finished")
	}
}
