package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/state"
)

func redeployReq(id int64, env, sha string, age time.Duration) gh.DeploymentRequest {
	r := gh.DeploymentRequest{ID: id, Environment: env, Sha: sha, CreatedAt: time.Now().Add(-age)}
	r.Creator.Login = "github-actions[bot]"
	r.Payload = []byte(`{"requested_by":"jan"}`)
	return r
}

func redeployPlan() *Plan {
	prod := ciTarget("c1")
	prod.Stack, prod.Env, prod.GitHubEnv = "api-prod", "prod", "prod"
	return &Plan{Targets: []Target{ciTarget("c1"), prod}}
}

func TestCollectRedeployRequests(t *testing.T) {
	git := &stubGit{redeploys: []gh.DeploymentRequest{
		redeployReq(5, "prod", "c1", time.Hour), // older than the controller knows the repo: ignored
		redeployReq(7, "sandbox", "c1", time.Minute),
		redeployReq(8, "staging", "c1", time.Minute),
	}}
	c := newTestController(t, appConfig(0), git)
	c.collectRedeployRequests(redeployPlan())

	var d state.Data
	c.st.View(func(s *state.Data) { d = *s })
	if !d.Force["api-sandbox"] || d.Force["api-prod"] {
		t.Fatalf("force = %v, want only api-sandbox", d.Force)
	}
	if got := d.Redeploys["api-sandbox"]; got.ID != 7 || got.By != "jan" || got.Sha != "c1" {
		t.Fatalf("redeploy = %+v", got)
	}
	if d.RedeployHandled["api"] != 8 {
		t.Fatalf("handled = %d, want 8", d.RedeployHandled["api"])
	}
	want := []string{
		"7 queued: Redeploy of api-sandbox requested by jan",
		"8 error: No stack deploys environment staging from this repository",
	}
	if !slices.Equal(git.statuses, want) {
		t.Fatalf("statuses = %q", git.statuses)
	}

	// Handled requests are not picked up again; a known repo takes older ones too.
	git.statuses = nil
	git.redeploys = append(git.redeploys, redeployReq(9, "sandbox", "c1", time.Hour))
	c.collectRedeployRequests(redeployPlan())
	want = []string{
		"7 inactive: Superseded by a newer redeploy request",
		"9 queued: Redeploy of api-sandbox requested by jan",
	}
	if !slices.Equal(git.statuses, want) {
		t.Fatalf("second scan statuses = %q", git.statuses)
	}
}

func TestCollectRedeployRequestsPaused(t *testing.T) {
	git := &stubGit{redeploys: []gh.DeploymentRequest{redeployReq(7, "sandbox", "c1", 0)}}
	c := newTestController(t, appConfig(0), git)
	_ = c.st.Update(func(d *state.Data) { d.Paused["api-sandbox"] = true })
	c.collectRedeployRequests(redeployPlan())

	var forced bool
	c.st.View(func(d *state.Data) { forced = d.Force["api-sandbox"] })
	if forced || len(git.statuses) != 1 || !strings.Contains(git.statuses[0], "7 error: Stack api-sandbox is paused") {
		t.Fatalf("forced=%v statuses=%q", forced, git.statuses)
	}
}

func TestRunReportsToRequestedDeployment(t *testing.T) {
	git := &stubGit{}
	c := newTestController(t, appConfig(0), git)
	_ = c.st.Update(func(d *state.Data) {
		d.Force["api-sandbox"] = true
		d.Redeploys["api-sandbox"] = state.Redeploy{Repo: "api", ID: 7, Sha: "c1", By: "jan"}
	})

	r := newRun(c, ciTarget("c1"), c.log)
	r.startDeployment(true)
	r.done("success", "Deployed", "", "")
	if git.deployments != 0 {
		t.Fatalf("created %d deployments, want the requested one reused", git.deployments)
	}
	want := []string{"7 in_progress: Redeploying c1 (requested by jan)", "7 success: Deployed"}
	if !slices.Equal(git.statuses, want) {
		t.Fatalf("statuses = %q", git.statuses)
	}
	c.st.View(func(d *state.Data) {
		if len(d.Redeploys) != 0 || d.Force["api-sandbox"] {
			t.Fatalf("request not cleared: %v %v", d.Redeploys, d.Force)
		}
	})
}

func TestRunReportsToRequestForOtherCommit(t *testing.T) {
	git := &stubGit{}
	c := newTestController(t, appConfig(0), git)
	_ = c.st.Update(func(d *state.Data) {
		d.Redeploys["api-sandbox"] = state.Redeploy{Repo: "api", ID: 7, Sha: "c0", By: "jan"}
	})

	r := newRun(c, ciTarget("c1"), c.log)
	r.startDeployment(true)
	r.done("failure", "Rollout failed", "", "")
	want := []string{"1001 in_progress: Redeploying c1", "1001 failure: Rollout failed", "7 failure: Rollout failed (commit c1)"}
	if !slices.Equal(git.statuses, want) {
		t.Fatalf("statuses = %q", git.statuses)
	}
}
