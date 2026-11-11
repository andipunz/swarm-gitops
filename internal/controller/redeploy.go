package controller

import (
	"fmt"
	"sort"
	"time"

	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/state"
)

// redeployMaxAge: when a repo's redeploy requests are read for the first
// time, older ones are ignored - they were made before the controller could
// handle them, and nobody is waiting for them any more.
const redeployMaxAge = 10 * time.Minute

// collectRedeployRequests turns new GitHub deployments with task "redeploy"
// into forced redeploys of the matching stacks. Anyone who can create a
// deployment (write access, e.g. via the redeploy workflow's "Run workflow"
// button) can restart a stack this way, but only with the commit and
// configuration it already deploys from git.
func (c *Controller) collectRedeployRequests(p *Plan) {
	byRepo := map[string][]Target{}
	for _, t := range p.Targets {
		byRepo[t.Repo] = append(byRepo[t.Repo], t)
	}
	for _, repo := range sortedKeys(byRepo) {
		reqs, err := c.git.RedeployRequests(repo)
		if err != nil {
			c.log.Warn("list redeploy requests failed", "repo", repo, "err", err)
			continue
		}
		var handled int64
		c.st.View(func(d *state.Data) { handled = d.RedeployHandled[repo] })
		sort.Slice(reqs, func(i, j int) bool { return reqs[i].ID < reqs[j].ID })
		newest := handled
		for _, req := range reqs {
			if req.ID <= handled {
				continue
			}
			newest = max(newest, req.ID)
			if handled == 0 && time.Since(req.CreatedAt) > redeployMaxAge {
				continue
			}
			c.handleRedeployRequest(repo, req, byRepo[repo])
		}
		if newest != handled {
			_ = c.st.Update(func(d *state.Data) { d.RedeployHandled[repo] = newest })
		}
	}
}

func (c *Controller) handleRedeployRequest(repo string, req gh.DeploymentRequest, targets []Target) {
	log := c.log.With("repo", repo, "environment", req.Environment, "deployment", req.ID, "by", req.RequestedBy())
	var t *Target
	for i := range targets {
		if targets[i].GitHubEnv == req.Environment {
			t = &targets[i]
		}
	}
	if t == nil {
		log.Warn("redeploy requested for an environment without a stack")
		_ = c.git.DeploymentStatus(repo, req.ID, "error", fmt.Sprintf("No stack deploys environment %s from this repository", req.Environment), "", "")
		return
	}
	var paused bool
	var superseded *state.Redeploy
	_ = c.st.Update(func(d *state.Data) {
		if paused = d.Paused[t.Stack]; paused {
			return
		}
		if prev, ok := d.Redeploys[t.Stack]; ok {
			superseded = &prev
		}
		d.Redeploys[t.Stack] = state.Redeploy{Repo: repo, ID: req.ID, Sha: req.Sha, By: req.RequestedBy()}
		d.Force[t.Stack] = true
	})
	if paused {
		log.Info("redeploy requested for a paused stack")
		_ = c.git.DeploymentStatus(repo, req.ID, "error", fmt.Sprintf("Stack %s is paused by an admin", t.Stack), "", "")
		return
	}
	if superseded != nil {
		_ = c.git.DeploymentStatus(repo, superseded.ID, "inactive", "Superseded by a newer redeploy request", "", "")
	}
	log.Info("redeploy requested", "stack", t.Stack)
	_ = c.git.DeploymentStatus(repo, req.ID, "queued", fmt.Sprintf("Redeploy of %s requested by %s", t.Stack, req.RequestedBy()), "", "")
}
