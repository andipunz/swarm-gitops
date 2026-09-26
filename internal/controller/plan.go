package controller

import (
	"fmt"
	"sort"

	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/spec"
)

// Target is one stack that should run: repo + environment + branch head.
type Target struct {
	Repo      string
	Env       string
	EnvCfg    spec.Env
	Branch    gh.Branch
	Stack     string
	GitHubEnv string
	Files     []string        // compose files, relative to .swarm/
	Optional  map[string]bool // files that may be missing
}

// StackInfo describes a stack currently running on the Swarm.
type StackInfo struct {
	Repo, Env, GitHubEnv string
}

// PlanInput is everything the planner needs; gathering it is the only part
// that talks to GitHub, so planning itself is pure and unit-tested.
type PlanInput struct {
	Repos      []gh.Repo
	Allowed    map[string][]string // repo -> enabled envs; nil = everything allowed
	Branches   map[string][]gh.Branch
	BranchErr  map[string]error
	SwarmFiles func(repo, tree string) (map[string]bool, error) // files inside .swarm/ of a tree
	Current    map[string]StackInfo
}

// Plan is the outcome of one scan.
type Plan struct {
	Targets      []Target
	Skipped      []Target          // env exists but is not enabled by an admin
	ConfigErrors map[string]error  // repo -> problem with deploy.yml (reported on default branch)
	Remove       map[string]string // stack -> reason (positive evidence only)
	Orphaned     map[string]string // stack -> why it is kept although nothing wants it
	Repos        map[string]gh.Repo
}

// MakePlan computes desired stacks and safe removals.
//
// A stack is only removed on positive evidence from a successful read:
// the repo is archived, or it is visible and its deploy.yml / environment /
// branch / .swarm dir / compose files are gone, or the environment is no
// longer enabled. Any read error keeps the stacks of that repo untouched.
func MakePlan(in PlanInput) *Plan {
	p := &Plan{ConfigErrors: map[string]error{}, Remove: map[string]string{}, Orphaned: map[string]string{}, Repos: map[string]gh.Repo{}}
	repos := p.Repos
	keepRepo := map[string]bool{}
	keepStack := map[string]bool{}
	gone := map[string]string{} // stack -> removal reason, when we saw it disappear
	desired := map[string]Target{}

	sorted := append([]gh.Repo{}, in.Repos...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, r := range sorted {
		repos[r.Name] = r
		if r.Archived || r.DeployFile == nil {
			continue
		}
		f, err := spec.Parse([]byte(*r.DeployFile))
		if err != nil {
			p.ConfigErrors[r.Name] = err
			keepRepo[r.Name] = true
			continue
		}
		if err := in.BranchErr[r.Name]; err != nil {
			keepRepo[r.Name] = true
			continue
		}
		for _, env := range f.EnvNames() {
			e := f.Environments[env]
			files, optional := e.ComposeFiles(env)
			for _, b := range in.Branches[r.Name] {
				if !e.Matches(b.Name) {
					continue
				}
				t := Target{
					Repo: r.Name, Env: env, EnvCfg: e, Branch: b,
					Stack:     spec.StackName(r.Name, env, e, b.Name),
					GitHubEnv: spec.GitHubEnvironment(env, e, b.Name),
					Files:     files, Optional: optional,
				}
				if b.SwarmTree == "" {
					gone[t.Stack] = fmt.Sprintf(".swarm/ no longer exists on branch %s", b.Name)
					continue
				}
				present, err := in.SwarmFiles(r.Name, b.SwarmTree)
				if err != nil {
					keepStack[t.Stack] = true
					continue
				}
				any := false
				for _, f := range files {
					any = any || present[f]
				}
				if !any {
					gone[t.Stack] = fmt.Sprintf("compose files of %s no longer exist on branch %s", env, b.Name)
					continue
				}
				if in.Allowed != nil && !contains(in.Allowed[r.Name], env) {
					gone[t.Stack] = fmt.Sprintf("environment %s is not enabled for this repository", env)
					p.Skipped = append(p.Skipped, t)
					continue
				}
				if other, dup := desired[t.Stack]; dup {
					p.ConfigErrors[r.Name] = fmt.Errorf("stack name %s is already used by %s/%s", t.Stack, other.Repo, other.Env)
					keepStack[t.Stack] = true
					continue
				}
				desired[t.Stack] = t
				p.Targets = append(p.Targets, t)
			}
		}
	}

	for _, stack := range sortedKeys(in.Current) {
		info := in.Current[stack]
		if _, ok := desired[stack]; ok || keepStack[stack] {
			continue
		}
		r, visible := repos[info.Repo]
		switch {
		case !visible:
			p.Orphaned[stack] = fmt.Sprintf("repository %s is not visible (deleted, renamed or app access removed) - kept, remove manually", info.Repo)
		case keepRepo[info.Repo]:
			// read error or broken deploy.yml: never remove on uncertainty
		case r.Archived:
			p.Remove[stack] = "repository archived"
		case r.DeployFile == nil:
			p.Remove[stack] = ".swarm/deploy.yml no longer exists on the default branch"
		case gone[stack] != "":
			p.Remove[stack] = gone[stack]
		default:
			p.Remove[stack] = "environment or branch no longer exists"
		}
	}
	return p
}

func contains(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
