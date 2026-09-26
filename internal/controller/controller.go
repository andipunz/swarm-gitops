// Package controller runs the reconcile loop: scan the org, deploy what
// changed, remove what is gone (safely), report everything back to GitHub.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andipunz/swarm-gitops/internal/config"
	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/policy"
	"github.com/andipunz/swarm-gitops/internal/prepare"
	"github.com/andipunz/swarm-gitops/internal/registry"
	"github.com/andipunz/swarm-gitops/internal/render"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

// Git is the subset of the GitHub client the controller uses (faked in tests).
type Git interface {
	ListRepos() ([]gh.Repo, error)
	Branches(repo string) ([]gh.Branch, error)
	EnvProperties(property string) (map[string][]string, error)
	Tree(repo, sha string) ([]gh.TreeEntry, error)
	Blob(repo, sha string) ([]byte, error)
	BranchProtected(repo, branch string) (bool, error)
	CreateCheckRun(repo, sha, name string) (*gh.CheckRun, error)
	CompleteCheckRun(repo string, id int64, conclusion, title, summary, text string) error
	CreateDeployment(repo, sha, env, description string, production, transient bool) (int64, error)
	DeploymentStatus(repo string, id int64, state, description, logURL, envURL string) error
	MarkEnvironmentInactive(repo, env, description string) error
}

// Controller is the reconcile loop.
type Controller struct {
	cfg    *config.Config
	git    Git
	docker *swarm.Docker
	reg    *registry.Resolver
	pol    *policy.Policy
	st     *state.Store
	log    *slog.Logger

	trigger chan struct{}
	locks   sync.Map // stack -> *sync.Mutex

	claimsMu sync.Mutex // held while checking hostnames + docker stack deploy

	cacheMu  sync.Mutex
	trees    map[string][]gh.TreeEntry // immutable, keyed by tree oid
	blobs    map[string][]byte         // immutable, keyed by blob sha
	attempts map[string]int            // stack@commit -> failed infrastructure attempts
}

// New creates a controller.
func New(cfg *config.Config, git Git, docker *swarm.Docker, reg *registry.Resolver, pol *policy.Policy, st *state.Store, log *slog.Logger) *Controller {
	return &Controller{
		cfg: cfg, git: git, docker: docker, reg: reg, pol: pol, st: st, log: log,
		trigger: make(chan struct{}, 1),
		trees:   map[string][]gh.TreeEntry{}, blobs: map[string][]byte{}, attempts: map[string]int{},
	}
}

// Run scans until ctx is cancelled. Image updates run in their own loop.
func (c *Controller) Run(ctx context.Context) {
	c.docker.RemovePrepLeftovers(ctx)
	if c.cfg.ImageInterval > 0 {
		go c.imageLoop(ctx)
	}
	for {
		c.Scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-c.trigger:
		case <-time.After(c.cfg.ScanInterval):
		}
	}
}

// Trigger requests an immediate scan.
func (c *Controller) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

func (c *Controller) lock(stack string) *sync.Mutex {
	m, _ := c.locks.LoadOrStore(stack, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Scan performs one full reconcile.
func (c *Controller) Scan(ctx context.Context) {
	start := time.Now()
	plan, err := c.buildPlan(ctx)
	_ = c.st.Update(func(d *state.Data) {
		d.LastScan = start
		d.LastScanError = ""
		if err != nil {
			d.LastScanError = err.Error()
		}
	})
	if err != nil {
		c.log.Error("scan failed, nothing changed", "err", err)
		return
	}
	c.reportConfigErrors(plan)
	c.reportSkipped(plan)
	c.removeStacks(ctx, plan)

	sem := make(chan struct{}, c.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, t := range plan.Targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(t Target) {
			defer wg.Done()
			defer func() { <-sem }()
			c.processTarget(ctx, t)
		}(t)
	}
	wg.Wait()
	c.log.Debug("scan done", "targets", len(plan.Targets), "took", time.Since(start).Round(time.Millisecond))
}

func (c *Controller) buildPlan(ctx context.Context) (*Plan, error) {
	repos, err := c.git.ListRepos()
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	var allowed map[string][]string
	if c.cfg.EnvProperty != "" {
		if allowed, err = c.git.EnvProperties(c.cfg.EnvProperty); err != nil {
			return nil, fmt.Errorf("read custom property %s: %w", c.cfg.EnvProperty, err)
		}
	}
	current, err := c.currentStacks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stacks: %w", err)
	}
	in := PlanInput{
		Repos: repos, Allowed: allowed, Current: current,
		Branches: map[string][]gh.Branch{}, BranchErr: map[string]error{},
		SwarmFiles: c.swarmFiles,
	}
	for _, r := range repos {
		if r.Archived || r.DeployFile == nil {
			continue
		}
		b, err := c.git.Branches(r.Name)
		if err != nil {
			c.log.Warn("list branches failed, keeping stacks of repo", "repo", r.Name, "err", err)
			in.BranchErr[r.Name] = err
			continue
		}
		in.Branches[r.Name] = b
	}
	plan := MakePlan(in)
	_ = c.st.Update(func(d *state.Data) { d.Orphaned = plan.Orphaned })
	for stack, why := range plan.Orphaned {
		c.log.Warn("orphaned stack kept", "stack", stack, "reason", why)
	}
	return plan, nil
}

func (c *Controller) currentStacks(ctx context.Context) (map[string]StackInfo, error) {
	stacks, err := c.docker.ManagedStacks(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]StackInfo{}
	for name, svcs := range stacks {
		s := svcs[0]
		res[name] = StackInfo{Repo: s.Label(render.LabelRepo), Env: s.Label(render.LabelEnv), GitHubEnv: s.Label(render.LabelGHEnv)}
	}
	return res, nil
}

func (c *Controller) tree(repo, oid string) ([]gh.TreeEntry, error) {
	c.cacheMu.Lock()
	t, ok := c.trees[oid]
	c.cacheMu.Unlock()
	if ok {
		return t, nil
	}
	t, err := c.git.Tree(repo, oid)
	if err != nil {
		return nil, err
	}
	c.cacheMu.Lock()
	if len(c.trees) > 2000 {
		c.trees = map[string][]gh.TreeEntry{}
	}
	c.trees[oid] = t
	c.cacheMu.Unlock()
	return t, nil
}

func (c *Controller) swarmFiles(repo, oid string) (map[string]bool, error) {
	t, err := c.tree(repo, oid)
	if err != nil {
		return nil, err
	}
	res := map[string]bool{}
	for _, e := range t {
		if e.Type == "blob" {
			res[e.Path] = true
		}
	}
	return res, nil
}

func (c *Controller) blob(repo, sha string) ([]byte, error) {
	c.cacheMu.Lock()
	b, ok := c.blobs[sha]
	c.cacheMu.Unlock()
	if ok {
		return b, nil
	}
	b, err := c.git.Blob(repo, sha)
	if err != nil {
		return nil, err
	}
	c.cacheMu.Lock()
	if len(c.blobs) > 5000 {
		c.blobs = map[string][]byte{}
	}
	c.blobs[sha] = b
	c.cacheMu.Unlock()
	return b, nil
}

// materialize writes the .swarm tree of a commit to dir.
func (c *Controller) materialize(repo, treeOid, dir string) error {
	entries, err := c.tree(repo, treeOid)
	if err != nil {
		return err
	}
	var total int64
	for _, e := range entries {
		switch {
		case e.Type == "tree":
			continue
		case e.Type != "blob" || e.Mode == "120000":
			return fmt.Errorf("%w: .swarm/%s: symlinks and submodules are not supported", render.ErrInvalid, e.Path)
		}
		clean := path.Clean(e.Path)
		if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || clean == "stack.rendered.yml" {
			return fmt.Errorf("%w: .swarm/%s: invalid path", render.ErrInvalid, e.Path)
		}
		if total += e.Size; total > 5<<20 {
			return fmt.Errorf("%w: .swarm/ is larger than 5 MB", render.ErrInvalid)
		}
		data, err := c.blob(repo, e.Sha)
		if err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) processTarget(ctx context.Context, t Target) {
	mu := c.lock(t.Stack)
	mu.Lock()
	defer mu.Unlock()

	var paused, force bool
	var processed string
	c.st.View(func(d *state.Data) {
		paused, force, processed = d.Paused[t.Stack], d.Force[t.Stack], d.Processed[t.Stack]
	})
	commit := t.Branch.Commit
	if paused || (processed == commit && !force) {
		return
	}
	redeploy := force || t.Branch.EmptyCommit()
	log := c.log.With("stack", t.Stack, "repo", t.Repo, "env", t.Env, "branch", t.Branch.Name, "commit", short(commit))
	log.Info("processing", "redeploy", redeploy)

	r := newRun(c, t, log)
	defer r.cleanup()

	// A stack with this name that we don't manage must never be overwritten.
	svcs, err := c.docker.StackServices(ctx, t.Stack)
	if err != nil {
		r.infraError(err)
		return
	}
	var adopted bool
	c.st.View(func(d *state.Data) { adopted = d.Adopted[t.Stack] })
	currentSpec := ""
	for _, s := range svcs {
		if s.Label(render.LabelManaged) != "true" && !adopted {
			r.fail("Stack name is taken", fmt.Sprintf("A stack named `%s` already exists on the Swarm and is not managed by swarm-gitops. An admin can take it over with `swarm-gitops adopt %s`.", t.Stack, t.Stack), "")
			return
		}
		if s.Label(render.LabelRepo) != "" && s.Label(render.LabelRepo) != t.Repo {
			r.fail("Stack name is taken", fmt.Sprintf("Stack `%s` belongs to repository %s.", t.Stack, s.Label(render.LabelRepo)), "")
			return
		}
		currentSpec = s.Label(render.LabelSpec)
	}

	if contains(c.cfg.RequireProtected, t.Env) {
		ok, err := c.git.BranchProtected(t.Repo, t.Branch.Name)
		if err != nil {
			r.infraError(err)
			return
		}
		if !ok {
			r.fail("Branch is not protected", fmt.Sprintf("Environment **%s** only deploys from protected branches. Protect `%s` (branch protection or a ruleset requiring pull requests).", t.Env, t.Branch.Name), "")
			return
		}
	}

	dir := filepath.Join(c.cfg.DataDir, "work", t.Stack, commit)
	_ = os.RemoveAll(dir)
	if err := c.materialize(t.Repo, t.Branch.SwarmTree, dir); err != nil {
		r.error(err)
		return
	}
	cl, err := c.currentClaims(ctx, t.Stack)
	if err != nil {
		r.infraError(err)
		return
	}
	res, err := render.Render(ctx, render.Input{
		Stack: t.Stack, Repo: t.Repo, Env: t.Env, GitHubEnv: t.GitHubEnv, Ref: t.Branch.Name, Commit: commit,
		URL: t.EnvCfg.URL, WorkDir: dir, Files: t.Files, Optional: t.Optional, VarsFile: t.EnvCfg.Vars, Policy: c.pol,
		HostClaims: cl.hosts, ObjectClaims: cl.objects,
	})
	if err != nil {
		r.error(err)
		return
	}
	if len(res.Violations) == 0 {
		var history map[string][]string
		c.st.View(func(d *state.Data) { history = copyHistory(d.BindHistory) })
		res.Violations = policy.CheckBindHistory(res.Binds, history)
	}
	if len(res.Violations) > 0 {
		r.fail(fmt.Sprintf("%d policy violation(s)", len(res.Violations)),
			"The stack was **not deployed** because it breaks the Swarm policy:\n\n- "+strings.Join(res.Violations, "\n- "), "")
		return
	}
	if currentSpec == res.SpecHash && len(svcs) == len(res.Services) && !redeploy {
		r.done("success", "No changes", fmt.Sprintf("Stack `%s` already runs this configuration. Push an empty commit to force a redeploy.", t.Stack), "")
		return
	}

	if c.cfg.DryRun {
		r.done("neutral", "Dry run: would deploy", fmt.Sprintf("Stack `%s` was rendered and passed the policy but was not deployed (DRY_RUN=true).", t.Stack), "")
		return
	}
	r.startDeployment(redeploy)
	if err := c.docker.Prepare(ctx, t.Stack, c.cfg.PrepImage, t.EnvCfg.BindOwner, prepJobs(res.Binds), c.cfg.PrepTimeout); err != nil {
		r.fail("Bind folders could not be prepared", "The stack was **not deployed**:\n\n```\n"+err.Error()+"\n```", "")
		return
	}
	// Hostnames and Traefik object names: re-check against the live Swarm
	// while holding the lock, so two stacks can't claim the same one concurrently.
	c.claimsMu.Lock()
	if conflict := c.claimConflicts(ctx, t.Stack, res.Hosts, res.Objects); conflict != "" {
		c.claimsMu.Unlock()
		r.fail("Hostname or Traefik name already in use", conflict, "")
		return
	}
	err = c.docker.Deploy(ctx, t.Stack, res.Path)
	c.claimsMu.Unlock()
	if err != nil {
		r.fail("docker stack deploy failed", "```\n"+err.Error()+"\n```", "")
		return
	}
	_ = c.st.Update(func(d *state.Data) {
		for root, srcs := range policy.WritableSources(res.Binds) {
			for _, src := range srcs {
				if !contains(d.BindHistory[root], src) {
					d.BindHistory[root] = append(d.BindHistory[root], src)
				}
			}
		}
	})
	if redeploy && currentSpec == res.SpecHash {
		after, _ := c.docker.StackServices(ctx, t.Stack)
		for _, s := range after {
			if err := c.docker.ForceUpdate(ctx, s.Spec.Name); err != nil {
				log.Warn("force update failed", "service", s.Spec.Name, "err", err)
			}
		}
	}
	ok, reports, err := c.docker.WaitRollout(ctx, t.Stack, c.cfg.RolloutTimeout)
	if err != nil {
		r.infraError(err)
		return
	}
	c.docker.PruneConfigs(ctx, t.Stack)
	summary, text := rolloutReport(reports)
	if ok {
		r.done("success", "Deployed", summary, text)
	} else {
		r.done("failure", "Rollout failed", summary, text)
	}
	c.cleanWorkdirs(t.Stack, commit)
}

// claims describes what is already taken on the live Swarm, by any service
// managed or not.
type claims struct {
	hosts   map[string]string // hostname -> owner
	objects map[string]string // Traefik object key ("proto.kind.name") -> owner
}

// currentClaims collects every hostname and Traefik object name routed on the
// Swarm, excluding the given stack.
func (c *Controller) currentClaims(ctx context.Context, exclude string) (claims, error) {
	svcs, err := c.docker.AllServices(ctx)
	if err != nil {
		return claims{}, err
	}
	cl := claims{hosts: map[string]string{}, objects: map[string]string{}}
	for _, s := range svcs {
		if s.Namespace() == exclude {
			continue
		}
		owner := s.Namespace()
		if owner == "" {
			owner = "service " + s.Spec.Name
		} else {
			owner = "stack " + owner
		}
		for _, h := range policy.HostsFromLabels(s.Spec.Labels) {
			if _, taken := cl.hosts[h]; !taken {
				cl.hosts[h] = owner
			}
		}
		for _, o := range policy.TraefikObjects(s.Spec.Labels) {
			if _, taken := cl.objects[o]; !taken {
				cl.objects[o] = owner
			}
		}
	}
	return cl, nil
}

func (c *Controller) claimConflicts(ctx context.Context, stack string, hosts, objects []string) string {
	if len(hosts) == 0 && len(objects) == 0 {
		return ""
	}
	cl, err := c.currentClaims(ctx, stack)
	if err != nil {
		return "could not verify hostnames and Traefik names: " + err.Error()
	}
	var msgs []string
	for _, h := range hosts {
		if owner, ok := cl.hosts[h]; ok {
			msgs = append(msgs, fmt.Sprintf("`%s` is already used by %s", h, owner))
		}
	}
	for _, o := range objects {
		if owner, ok := cl.objects[o]; ok {
			msgs = append(msgs, fmt.Sprintf("Traefik %s is already used by %s", objectDesc(o), owner))
		}
	}
	return strings.Join(msgs, "\n")
}

// objectDesc renders a Traefik object key ("proto.kind.name") for a report.
func objectDesc(key string) string {
	parts := strings.SplitN(key, ".", 3)
	if len(parts) != 3 {
		return key
	}
	return fmt.Sprintf("%s `%s` (%s)", policy.TraefikKindLabel(parts[1]), parts[2], parts[0])
}

// prepJobs groups bind folders by placement constraints: one job per group.
// A prefix is mounted read-only into the job when none of its specs need to
// create anything below it (prepare.Run then never touches the filesystem,
// just os.Lstat) - the most important case being a bind rule on "/" itself.
func prepJobs(binds []policy.Bind) []swarm.PrepJob {
	type build struct {
		constraints  []string
		prefixOrder  []string
		prefixCreate map[string]bool
		specs        []string
	}
	byKey := map[string]*build{}
	var keys []string
	for _, b := range binds {
		key := strings.Join(b.Constraints, "\x00")
		j := byKey[key]
		if j == nil {
			j = &build{constraints: b.Constraints, prefixCreate: map[string]bool{}}
			byKey[key] = j
			keys = append(keys, key)
		}
		if _, ok := j.prefixCreate[b.Prefix]; !ok {
			j.prefixOrder = append(j.prefixOrder, b.Prefix)
		}
		j.prefixCreate[b.Prefix] = j.prefixCreate[b.Prefix] || b.Create
		spec := prepare.Spec{Prefix: b.Prefix, Path: b.Source, Create: b.Create}.String()
		if !contains(j.specs, spec) {
			j.specs = append(j.specs, spec)
		}
	}
	sort.Strings(keys)
	var jobs []swarm.PrepJob
	for _, k := range keys {
		b := byKey[k]
		job := swarm.PrepJob{Constraints: b.constraints, Specs: b.specs}
		for _, p := range b.prefixOrder {
			job.Prefixes = append(job.Prefixes, swarm.PrepPrefix{Path: p, ReadOnly: !b.prefixCreate[p]})
		}
		jobs = append(jobs, job)
	}
	return jobs
}

func copyHistory(h map[string][]string) map[string][]string {
	res := make(map[string][]string, len(h))
	for k, v := range h {
		res[k] = append([]string(nil), v...)
	}
	return res
}

func (c *Controller) cleanWorkdirs(stack, keep string) {
	base := filepath.Join(c.cfg.DataDir, "work", stack)
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if e.Name() != keep {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

func (c *Controller) reportConfigErrors(p *Plan) {
	for repo, err := range p.ConfigErrors {
		r, ok := p.Repos[repo]
		if !ok || r.DefaultHead == "" {
			continue
		}
		key := "config:" + repo
		var done bool
		c.st.View(func(d *state.Data) { done = d.Processed[key] == r.DefaultHead })
		if done {
			continue
		}
		c.log.Warn("invalid deploy configuration", "repo", repo, "err", err)
		if cr, cerr := c.git.CreateCheckRun(repo, r.DefaultHead, "swarm / deploy.yml"); cerr == nil {
			_ = c.git.CompleteCheckRun(repo, cr.ID, "failure", "Invalid .swarm/deploy.yml",
				"Nothing was deployed or removed for this repository until this is fixed.\n\n```\n"+err.Error()+"\n```", "")
		}
		_ = c.st.Update(func(d *state.Data) { d.Processed[key] = r.DefaultHead })
	}
}

func (c *Controller) reportSkipped(p *Plan) {
	for _, t := range p.Skipped {
		key := "skipped:" + t.Stack
		var done bool
		c.st.View(func(d *state.Data) { done = d.Processed[key] == t.Branch.Commit })
		if done {
			continue
		}
		if cr, err := c.git.CreateCheckRun(t.Repo, t.Branch.Commit, "swarm / "+t.GitHubEnv); err == nil {
			_ = c.git.CompleteCheckRun(t.Repo, cr.ID, "neutral", fmt.Sprintf("Environment %s is not enabled", t.Env),
				fmt.Sprintf("`%s` is defined in `.swarm/deploy.yml` but not enabled for this repository. An org admin enables it via the custom property **%s**.", t.Env, c.cfg.EnvProperty), "")
		}
		_ = c.st.Update(func(d *state.Data) { d.Processed[key] = t.Branch.Commit })
	}
}

// removeStacks applies removals only after PRUNE_CONFIRMATIONS consecutive
// scans agree, and stops for approval if too many stacks would go at once.
func (c *Controller) removeStacks(ctx context.Context, p *Plan) {
	var ready []string
	var approved bool
	var paused map[string]bool
	_ = c.st.Update(func(d *state.Data) {
		for stack := range d.PendingRemoval {
			if _, still := p.Remove[stack]; !still {
				delete(d.PendingRemoval, stack)
			}
		}
		for _, stack := range sortedKeys(p.Remove) {
			d.PendingRemoval[stack]++
			if d.PendingRemoval[stack] >= c.cfg.PruneConfirm {
				ready = append(ready, stack)
			}
		}
		approved = d.PruneApproved
		paused = map[string]bool{}
		for k, v := range d.Paused {
			paused[k] = v
		}
		d.PruneBlocked = nil
	})
	if len(ready) == 0 {
		return
	}
	if !c.cfg.PruneEnabled || c.cfg.DryRun {
		c.log.Warn("removal disabled (PRUNE_ENABLED=false)", "stacks", ready)
		_ = c.st.Update(func(d *state.Data) { d.PruneBlocked = ready })
		return
	}
	if len(ready) > c.cfg.MaxPrune && !approved {
		c.log.Error("too many stacks to remove at once, waiting for approval: swarm-gitops approve-prune", "stacks", ready, "max", c.cfg.MaxPrune)
		_ = c.st.Update(func(d *state.Data) { d.PruneBlocked = ready })
		return
	}
	current, _ := c.currentStacks(ctx)
	for _, stack := range ready {
		if paused[stack] {
			continue
		}
		mu := c.lock(stack)
		mu.Lock()
		reason := p.Remove[stack]
		c.log.Info("removing stack", "stack", stack, "reason", reason)
		if err := c.docker.Remove(ctx, stack); err != nil {
			c.log.Error("remove stack failed", "stack", stack, "err", err)
			mu.Unlock()
			continue
		}
		if info, ok := current[stack]; ok && info.GitHubEnv != "" && !strings.Contains(reason, "archived") {
			if err := c.git.MarkEnvironmentInactive(info.Repo, info.GitHubEnv, "Stack removed: "+reason); err != nil {
				c.log.Warn("mark deployment inactive failed", "stack", stack, "err", err)
			}
		}
		_ = c.st.Update(func(d *state.Data) {
			delete(d.PendingRemoval, stack)
			delete(d.Processed, stack)
			delete(d.Force, stack)
		})
		mu.Unlock()
	}
	_ = c.st.Update(func(d *state.Data) { d.PruneApproved = false })
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
