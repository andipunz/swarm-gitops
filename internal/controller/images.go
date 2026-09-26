package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/andipunz/swarm-gitops/internal/registry"
	"github.com/andipunz/swarm-gitops/internal/render"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

func (c *Controller) imageLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.ImageInterval):
		}
		c.CheckImages(ctx)
	}
}

// CheckImages redeploys services whose image tag now points to a new digest.
// Images pinned by digest in the compose file are never touched.
func (c *Controller) CheckImages(ctx context.Context) {
	stacks, err := c.docker.ManagedStacks(ctx)
	if err != nil {
		c.log.Error("image check: list stacks", "err", err)
		return
	}
	digests := map[string]string{}
	failed := map[string]bool{}
	resolve := func(img string) (string, bool) {
		if d, ok := digests[img]; ok {
			return d, true
		}
		if failed[img] {
			return "", false
		}
		d, err := c.reg.Digest(registry.Parse(img))
		if err != nil {
			c.log.Warn("image check: resolve failed", "image", img, "err", err)
			failed[img] = true
			return "", false
		}
		digests[img] = d
		return d, true
	}
	for _, stack := range sortedKeys(stacks) {
		var paused bool
		c.st.View(func(d *state.Data) { paused = d.Paused[stack] })
		if paused {
			continue
		}
		mu := c.lock(stack)
		if !mu.TryLock() {
			continue // a deployment is running; check again next round
		}
		c.updateStackImages(ctx, stack, stacks[stack], resolve)
		mu.Unlock()
	}
	_ = c.st.Update(func(d *state.Data) { d.LastImageCheck = time.Now() })
}

func (c *Controller) updateStackImages(ctx context.Context, stack string, svcs []swarm.Service, resolve func(string) (string, bool)) {
	var changed []string
	for _, s := range svcs {
		img := s.Label(render.LabelImage)
		if img == "" || strings.Contains(img, "@") {
			continue
		}
		want, ok := resolve(img)
		if !ok {
			continue
		}
		if registry.Parse(s.Spec.TaskTemplate.ContainerSpec.Image).Digest == want {
			continue
		}
		c.log.Info("new image", "stack", stack, "service", s.Spec.Name, "image", img, "digest", want)
		if err := c.docker.UpdateImage(ctx, s.Spec.Name, img+"@"+want); err != nil {
			c.log.Error("image update failed", "service", s.Spec.Name, "err", err)
			continue
		}
		changed = append(changed, fmt.Sprintf("%s → %s", strings.TrimPrefix(s.Spec.Name, stack+"_"), want[:min(19, len(want))]))
	}
	if len(changed) == 0 {
		return
	}
	s := svcs[0]
	repo, commit, ghEnv, url := s.Label(render.LabelRepo), s.Label(render.LabelCommit), s.Label(render.LabelGHEnv), s.Label(render.LabelURL)
	desc := "New image: " + strings.Join(changed, ", ")
	env := s.Label(render.LabelEnv)
	depID, err := c.git.CreateDeployment(repo, commit, ghEnv, desc, env == "prod" || env == "production", strings.Contains(ghEnv, "/"))
	if err != nil {
		c.log.Warn("create deployment failed", "stack", stack, "err", err)
	} else {
		_ = c.git.DeploymentStatus(repo, depID, "in_progress", desc, "", url)
	}
	ok, reports, err := c.docker.WaitRollout(ctx, stack, c.cfg.RolloutTimeout)
	if err != nil {
		c.log.Error("image update rollout", "stack", stack, "err", err)
		return
	}
	state, msg := "success", desc
	if !ok {
		state, msg = "failure", "Image update failed (Swarm rolled back where configured): "+strings.Join(changed, ", ")
		for _, r := range reports {
			if r.State != "running" {
				c.log.Error("image update rollout failed", "service", r.Name, "state", r.State, "error", r.Error)
			}
		}
	}
	if depID != 0 {
		_ = c.git.DeploymentStatus(repo, depID, state, msg, "", url)
	}
	c.log.Info("image update done", "stack", stack, "result", state)
}
