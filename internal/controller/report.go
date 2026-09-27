package controller

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/render"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

// maxAttempts for temporary errors (GitHub/Docker unreachable) per commit.
const maxAttempts = 3

// run tracks the GitHub feedback for processing one target.
type run struct {
	c        *Controller
	t        Target
	log      *slog.Logger
	check    *gh.CheckRun
	depID    int64
	finished bool
}

func newRun(c *Controller, t Target, log *slog.Logger) *run {
	r := &run{c: c, t: t, log: log}
	// The Checks API is GitHub-App-only - a PAT gets a 403 on every single
	// call, not a permission that can be granted to it. Skip creating one
	// entirely under token auth instead of warning on every scan; done()
	// logs the result detail instead so it isn't silently lost.
	if c.cfg.Token != "" {
		return r
	}
	cr, err := c.git.CreateCheckRun(t.Repo, t.Branch.Commit, "swarm / "+t.GitHubEnv)
	if err != nil {
		log.Warn("create check run failed", "err", err)
	} else {
		r.check = cr
	}
	return r
}

func (r *run) header() string {
	return fmt.Sprintf("**Stack** `%s` · **Environment** `%s` · **Branch** `%s` · **Commit** `%s`\n\n",
		r.t.Stack, r.t.GitHubEnv, r.t.Branch.Name, short(r.t.Branch.Commit))
}

func (r *run) startDeployment(redeploy bool) {
	desc := "Deploying " + short(r.t.Branch.Commit)
	if redeploy {
		desc = "Redeploying " + short(r.t.Branch.Commit)
	}
	prod := r.t.Env == "prod" || r.t.Env == "production"
	id, err := r.c.git.CreateDeployment(r.t.Repo, r.t.Branch.Commit, r.t.GitHubEnv, desc, prod, r.t.EnvCfg.IsGlob())
	if err != nil {
		r.log.Warn("create deployment failed", "err", err)
		return
	}
	r.depID = id
	_ = r.c.git.DeploymentStatus(r.t.Repo, id, "in_progress", desc, r.logURL(), r.t.EnvCfg.URL)
}

func (r *run) logURL() string {
	if r.check != nil {
		return r.check.HTMLURL
	}
	return ""
}

// done finishes the check run and deployment and marks the commit processed.
func (r *run) done(conclusion, title, summary, text string) {
	r.finished = true
	if r.check != nil {
		if err := r.c.git.CompleteCheckRun(r.t.Repo, r.check.ID, conclusion, title, r.header()+summary, text); err != nil {
			r.log.Warn("complete check run failed", "err", err)
		}
	}
	if r.depID != 0 {
		state := "failure"
		if conclusion == "success" {
			state = "success"
		}
		_ = r.c.git.DeploymentStatus(r.t.Repo, r.depID, state, title, r.logURL(), r.t.EnvCfg.URL)
	}
	key := r.t.Stack + "@" + r.t.Branch.Commit
	r.c.cacheMu.Lock()
	delete(r.c.attempts, key)
	r.c.cacheMu.Unlock()
	_ = r.c.st.Update(func(d *state.Data) {
		d.Processed[r.t.Stack] = r.t.Branch.Commit
		delete(d.Force, r.t.Stack)
	})
	if r.check == nil {
		// No check run to carry summary/text (token auth, or creation
		// failed) - log it instead, or e.g. a policy violation's detail
		// would otherwise never surface anywhere.
		r.log.Info("done", "result", conclusion, "title", title, "summary", summary)
	} else {
		r.log.Info("done", "result", conclusion, "title", title)
	}
}

func (r *run) fail(title, summary, text string) { r.done("failure", title, summary, text) }

// error reports a problem: repo content errors are final, others are retried.
func (r *run) error(err error) {
	if errors.Is(err, render.ErrInvalid) {
		msg := strings.TrimPrefix(err.Error(), render.ErrInvalid.Error()+": ")
		// never show controller-internal paths to devs
		msg = strings.ReplaceAll(msg, r.c.cfg.DataDir+"/work/"+r.t.Stack+"/"+r.t.Branch.Commit+"/", ".swarm/")
		msg = strings.ReplaceAll(msg, r.c.cfg.DataDir, "<data>")
		r.fail("Invalid stack", "The stack was **not deployed**:\n\n```\n"+msg+"\n```", "")
		return
	}
	r.infraError(err)
}

// infraError is retried on the next scans; after maxAttempts the commit is
// marked processed (push an empty commit or use the CLI to retry later).
func (r *run) infraError(err error) {
	key := r.t.Stack + "@" + r.t.Branch.Commit
	r.c.cacheMu.Lock()
	r.c.attempts[key]++
	n := r.c.attempts[key]
	r.c.cacheMu.Unlock()
	r.log.Error("deployment error", "attempt", n, "err", err)
	if n >= maxAttempts {
		r.fail("Deployment error", fmt.Sprintf("Giving up after %d attempts. Push an empty commit to retry.\n\n```\n%v\n```", n, err), "")
		return
	}
	r.finished = true
	if r.check != nil {
		_ = r.c.git.CompleteCheckRun(r.t.Repo, r.check.ID, "neutral", fmt.Sprintf("Temporary error, retrying (%d/%d)", n, maxAttempts),
			r.header()+"```\n"+err.Error()+"\n```", "")
	}
	if r.depID != 0 {
		_ = r.c.git.DeploymentStatus(r.t.Repo, r.depID, "error", "Temporary error, retrying", r.logURL(), "")
	}
}

// cleanup makes sure no check run stays "in progress" forever.
func (r *run) cleanup() {
	if !r.finished && r.check != nil {
		_ = r.c.git.CompleteCheckRun(r.t.Repo, r.check.ID, "cancelled", "Aborted", r.header(), "")
	}
}

// rolloutReport renders the service table and logs of failed services.
func rolloutReport(reports []swarm.ServiceReport) (summary, text string) {
	var b, t strings.Builder
	b.WriteString("| Service | Image | Replicas | State |\n|---|---|---|---|\n")
	for _, r := range reports {
		desired := fmt.Sprint(r.Desired)
		if r.Desired < 0 {
			desired = "global"
		}
		img := r.Image
		if i := strings.Index(img, "@sha256:"); i >= 0 {
			img = img[:i] + "@" + img[i+1:i+20] + "…"
		}
		state := r.State
		if r.State == "running" {
			state = "✅ running"
		} else {
			state = "❌ " + state
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %d/%s | %s |\n", r.Name, img, r.Running, desired, state)
		if r.Error != "" || r.Logs != "" {
			fmt.Fprintf(&t, "### %s\n", r.Name)
			if r.Error != "" {
				fmt.Fprintf(&t, "**Error:** `%s`\n\n", r.Error)
			}
			if r.Logs != "" {
				fmt.Fprintf(&t, "Last log lines:\n```\n%s\n```\n", strings.TrimSpace(r.Logs))
			}
		}
	}
	return b.String(), t.String()
}
