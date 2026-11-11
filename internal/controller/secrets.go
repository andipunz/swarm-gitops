package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/andipunz/swarm-gitops/internal/render"
	"github.com/andipunz/swarm-gitops/internal/secretdriver"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

// pendingSecret is a changed secret version waiting for the plugin caches on
// all managers to expire, so restarted tasks really get the new value.
type pendingSecret struct {
	version string
	since   time.Time
}

func (c *Controller) secretLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.SecretInterval):
		}
		c.CheckSecrets(ctx)
	}
}

// secretKey identifies one secret of one stack in state.SecretVersions.
func secretKey(stack, secret string) string { return stack + "|" + secret }

// secretAction is what to do about one secret's current version.
type secretAction int

const (
	secretUnchanged secretAction = iota
	secretRecord                 // first seen: remember the version, restart nothing
	secretWait                   // changed, but other managers may still cache the old value
	secretRestart                // changed and settled: restart the services using it
)

// secretDecision compares a secret's current version with the one its
// services last started with. A change only restarts anything once it was
// seen for settle (the plugin's cache TTL): the task is scheduled by the
// leader, whose plugin may still hand out the old value until then.
func secretDecision(known bool, old, current string, pending pendingSecret, now time.Time, settle time.Duration) secretAction {
	switch {
	case !known:
		return secretRecord
	case old == current:
		return secretUnchanged
	case pending.version != current || now.Sub(pending.since) < settle:
		return secretWait
	default:
		return secretRestart
	}
}

// CheckSecrets restarts services whose driver secrets changed at the source
// (e.g. an item edited in 1Password). Swarm resolves a driver secret only
// when a task starts, so running tasks would otherwise keep the old value.
// Secrets of plugins that don't answer version requests are skipped.
func (c *Controller) CheckSecrets(ctx context.Context) {
	defer func() { _ = c.st.Update(func(d *state.Data) { d.LastSecretCheck = time.Now() }) }()
	secrets, err := c.docker.DriverSecrets(ctx)
	if err != nil {
		c.log.Error("secret check: list secrets", "err", err)
		return
	}
	if len(secrets) == 0 {
		return
	}
	stacks, err := c.docker.ManagedStacks(ctx)
	if err != nil {
		c.log.Error("secret check: list stacks", "err", err)
		return
	}

	// Driver secrets used by managed stacks, per driver.
	used := map[string]map[string]bool{} // stack -> secret names
	byDriver := map[string][]secretdriver.Secret{}
	queued := map[string]bool{}
	for stack, svcs := range stacks {
		for _, s := range svcs {
			for _, ref := range s.Spec.TaskTemplate.ContainerSpec.Secrets {
				sec, ok := secrets[ref.SecretName]
				if !ok {
					continue
				}
				if used[stack] == nil {
					used[stack] = map[string]bool{}
				}
				used[stack][sec.Spec.Name] = true
				if !queued[sec.Spec.Name] {
					queued[sec.Spec.Name] = true
					byDriver[sec.Spec.Driver.Name] = append(byDriver[sec.Spec.Driver.Name], secretdriver.Secret{Name: sec.Spec.Name, Labels: sec.Spec.Labels})
				}
			}
		}
	}

	versions := map[string]string{}
	settle := map[string]time.Duration{}
	for _, driver := range sortedKeys(byDriver) {
		res, err := c.driverVersions(ctx, driver, byDriver[driver])
		if err != nil {
			continue
		}
		for name, v := range res.Versions {
			versions[name], settle[name] = v, res.CacheTTL
		}
		for name, msg := range res.Errors {
			c.log.Warn("secret check: no version", "secret", name, "driver", driver, "err", msg)
		}
	}

	now := time.Now()
	changed := map[string][]string{} // stack -> changed secret names
	_ = c.st.Update(func(d *state.Data) {
		for key := range d.SecretVersions {
			stack, name, _ := strings.Cut(key, "|")
			if !used[stack][name] {
				delete(d.SecretVersions, key)
			}
		}
		for _, stack := range sortedKeys(used) {
			if d.Paused[stack] {
				continue
			}
			for _, name := range sortedKeys(used[stack]) {
				current, ok := versions[name]
				if !ok {
					continue
				}
				key := secretKey(stack, name)
				old, known := d.SecretVersions[key]
				c.cacheMu.Lock()
				p := c.pendingSecrets[key]
				switch secretDecision(known, old, current, p, now, settle[name]) {
				case secretRecord:
					d.SecretVersions[key] = current
				case secretUnchanged:
					delete(c.pendingSecrets, key)
				case secretWait:
					if p.version != current {
						c.pendingSecrets[key] = pendingSecret{version: current, since: now}
						c.log.Info("secret changed, waiting for plugin caches to expire", "stack", stack, "secret", name, "wait", settle[name])
					}
				case secretRestart:
					changed[stack] = append(changed[stack], name)
				}
				c.cacheMu.Unlock()
			}
		}
	})

	for _, stack := range sortedKeys(changed) {
		mu := c.lock(stack)
		if !mu.TryLock() {
			continue // a deployment is running; the change stays pending until next round
		}
		svcs, err := c.docker.StackServices(ctx, stack)
		if err != nil {
			c.log.Error("secret check: list services", "stack", stack, "err", err)
		} else if len(svcs) > 0 {
			c.restartForSecrets(ctx, stack, svcs, changed[stack], versions)
		}
		mu.Unlock()
	}
}

// driverVersions asks one secret driver plugin for its secrets' versions.
func (c *Controller) driverVersions(ctx context.Context, driver string, secrets []secretdriver.Secret) (*secretdriver.Result, error) {
	socket, err := c.docker.PluginSocket(ctx, c.cfg.PluginDir, driver)
	if err == nil {
		var res *secretdriver.Result
		if res, err = secretdriver.Versions(ctx, socket, secrets); err == nil {
			return res, nil
		}
	}
	if errors.Is(err, secretdriver.ErrUnsupported) {
		c.log.Debug("secret check: driver does not report versions", "driver", driver)
	} else {
		c.log.Warn("secret check: driver unreachable", "driver", driver, "err", err)
	}
	return nil, err
}

// restartForSecrets force-updates the services of a stack using any of the
// changed secrets and reports it as a deployment on GitHub, like a new image.
func (c *Controller) restartForSecrets(ctx context.Context, stack string, svcs []swarm.Service, changed []string, versions map[string]string) {
	isChanged := map[string]bool{}
	for _, name := range changed {
		isChanged[name] = true
	}
	var restarted []string
	for _, s := range svcs {
		for _, ref := range s.Spec.TaskTemplate.ContainerSpec.Secrets {
			if !isChanged[ref.SecretName] {
				continue
			}
			c.log.Info("secret changed, restarting service", "stack", stack, "service", s.Spec.Name)
			if err := c.docker.ForceUpdate(ctx, s.Spec.Name); err != nil {
				c.log.Error("secret restart failed", "service", s.Spec.Name, "err", err)
			} else {
				restarted = append(restarted, strings.TrimPrefix(s.Spec.Name, stack+"_"))
			}
			break
		}
	}
	// Remember the new versions even if a restart failed: retrying every
	// round would only restart the healthy services again and again.
	_ = c.st.Update(func(d *state.Data) {
		for _, name := range changed {
			d.SecretVersions[secretKey(stack, name)] = versions[name]
		}
	})
	c.cacheMu.Lock()
	for _, name := range changed {
		delete(c.pendingSecrets, secretKey(stack, name))
	}
	c.cacheMu.Unlock()
	if len(restarted) == 0 {
		return
	}

	short := make([]string, 0, len(changed))
	for _, name := range changed {
		short = append(short, strings.TrimPrefix(name, stack+"_"))
	}
	sort.Strings(short)
	sort.Strings(restarted)
	s := svcs[0]
	repo, commit, ghEnv, url := s.Label(render.LabelRepo), s.Label(render.LabelCommit), s.Label(render.LabelGHEnv), s.Label(render.LabelURL)
	desc := fmt.Sprintf("Secret changed: %s → restarted %s", strings.Join(short, ", "), strings.Join(restarted, ", "))
	env := s.Label(render.LabelEnv)
	depID, err := c.git.CreateDeployment(repo, commit, ghEnv, desc, env == "prod" || env == "production", strings.Contains(ghEnv, "/"))
	if err != nil {
		c.log.Warn("create deployment failed", "stack", stack, "err", err)
	} else {
		_ = c.git.DeploymentStatus(repo, depID, "in_progress", desc, "", url)
	}
	ok, reports, err := c.docker.WaitRollout(ctx, stack, c.cfg.RolloutTimeout)
	if err != nil {
		c.log.Error("secret restart rollout", "stack", stack, "err", err)
		return
	}
	result, msg := "success", desc
	if !ok {
		result, msg = "failure", "Restart after secret change failed: "+strings.Join(restarted, ", ")
		for _, r := range reports {
			if r.State != "running" {
				c.log.Error("secret restart rollout failed", "service", r.Name, "state", r.State, "error", r.Error)
			}
		}
	}
	if depID != 0 {
		_ = c.git.DeploymentStatus(repo, depID, result, msg, "", url)
	}
	c.log.Info("secret restart done", "stack", stack, "result", result)
}
