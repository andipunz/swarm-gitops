package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andipunz/swarm-gitops/internal/config"
	"github.com/andipunz/swarm-gitops/internal/gh"
)

func ciTarget(commit string) Target {
	return Target{Stack: "api-sandbox", Repo: "api", Env: "sandbox", GitHubEnv: "sandbox", Branch: gh.Branch{Name: "develop", Commit: commit}}
}

func appConfig(wait time.Duration) *config.Config {
	return &config.Config{AppID: 42, AppKeyFile: "/key.pem", CIWait: wait}
}

func build(status, conclusion string) gh.CheckRunStatus {
	return gh.CheckRunStatus{Name: "docker / build-publish", Status: status, Conclusion: conclusion, AppID: 15368}
}

func TestWaitForCIWaitsForRunningBuild(t *testing.T) {
	git := &stubGit{runs: []gh.CheckRunStatus{build("in_progress", "")}}
	c := newTestController(t, appConfig(30*time.Minute), git)

	for i := 0; i < 2; i++ {
		if wait, _ := c.waitForCI(ciTarget("c1"), c.log); !wait {
			t.Fatalf("scan %d: deployed while the build was still running", i+1)
		}
	}
	git.runs = []gh.CheckRunStatus{build("completed", "success")}
	wait, note := c.waitForCI(ciTarget("c1"), c.log)
	if wait || note != "" {
		t.Fatalf("after a successful build: wait=%v note=%q, want deploy without note", wait, note)
	}
	if len(c.ciSince) != 0 {
		t.Fatalf("waiting state not cleaned up: %v", c.ciSince)
	}
}

func TestWaitForCIIgnoresOwnCheckRuns(t *testing.T) {
	git := &stubGit{runs: []gh.CheckRunStatus{
		{Name: "swarm / sandbox", Status: "in_progress", AppID: 42},
		{Name: "something else by this app", Status: "queued", AppID: 42},
	}}
	c := newTestController(t, appConfig(30*time.Minute), git)

	// Only our own runs means no CI: one grace scan, then deploy.
	if wait, _ := c.waitForCI(ciTarget("c1"), c.log); !wait {
		t.Fatal("first scan: expected a grace scan for check runs to show up")
	}
	if wait, note := c.waitForCI(ciTarget("c1"), c.log); wait || note != "" {
		t.Fatalf("second scan: wait=%v note=%q, want deploy", wait, note)
	}
}

func TestWaitForCIDeploysAfterTimeout(t *testing.T) {
	git := &stubGit{runs: []gh.CheckRunStatus{build("queued", "")}}
	c := newTestController(t, appConfig(10*time.Minute), git)

	if wait, _ := c.waitForCI(ciTarget("c1"), c.log); !wait {
		t.Fatal("expected to wait at first")
	}
	c.ciSince["api-sandbox@c1"] = time.Now().Add(-11 * time.Minute)
	wait, note := c.waitForCI(ciTarget("c1"), c.log)
	if wait {
		t.Fatal("still waiting after CI_WAIT_TIMEOUT")
	}
	if !strings.Contains(note, "docker / build-publish") {
		t.Fatalf("note should name the unfinished check, got %q", note)
	}
}

func TestWaitForCIDeploysDespiteFailedCI(t *testing.T) {
	git := &stubGit{runs: []gh.CheckRunStatus{build("completed", "failure"), {Name: "lint", Status: "completed", Conclusion: "skipped"}}}
	c := newTestController(t, appConfig(30*time.Minute), git)

	wait, note := c.waitForCI(ciTarget("c1"), c.log)
	if wait {
		t.Fatal("a finished (failed) CI must not block the deploy")
	}
	if !strings.Contains(note, "docker / build-publish (failure)") || strings.Contains(note, "lint") {
		t.Fatalf("note should list only the failed check, got %q", note)
	}
}

func TestWaitForCIRetriesWhenStatusUnreadable(t *testing.T) {
	git := &stubGit{runsErr: errors.New("502 Bad Gateway")}
	c := newTestController(t, appConfig(30*time.Minute), git)

	if wait, _ := c.waitForCI(ciTarget("c1"), c.log); !wait {
		t.Fatal("expected to retry on the next scan when the CI status can't be read")
	}
	c.ciSince["api-sandbox@c1"] = time.Now().Add(-31 * time.Minute)
	if wait, note := c.waitForCI(ciTarget("c1"), c.log); wait || !strings.Contains(note, "502") {
		t.Fatalf("after the timeout: wait=%v note=%q, want deploy noting the error", wait, note)
	}
}

func TestWaitForCIDisabled(t *testing.T) {
	git := &stubGit{runs: []gh.CheckRunStatus{build("in_progress", "")}}
	for name, cfg := range map[string]*config.Config{
		"CI_WAIT_TIMEOUT=0": appConfig(0),
		"token auth":        {Token: "ghp_x", CIWait: 30 * time.Minute},
	} {
		c := newTestController(t, cfg, git)
		if wait, note := c.waitForCI(ciTarget("c1"), c.log); wait || note != "" {
			t.Errorf("%s: wait=%v note=%q, want immediate deploy", name, wait, note)
		}
	}
}

func TestWaitForCIForgetsSupersededCommit(t *testing.T) {
	git := &stubGit{runs: []gh.CheckRunStatus{build("in_progress", "")}}
	c := newTestController(t, appConfig(30*time.Minute), git)

	c.waitForCI(ciTarget("old"), c.log)
	c.waitForCI(ciTarget("new"), c.log)
	if _, ok := c.ciSince["api-sandbox@old"]; ok {
		t.Fatal("the superseded commit's waiting state was kept")
	}
	if _, ok := c.ciSince["api-sandbox@new"]; !ok {
		t.Fatal("the new commit is not tracked as waiting")
	}
}
