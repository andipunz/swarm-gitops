package policy

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/andipunz/swarm-gitops/internal/spec"
)

// BindRule allows bind mounts below a host folder.
//
//	bind_mounts:
//	  - path: /srv/swarm/{repo}/{env}   # {repo}, {env}, {stack} are filled in per stack
//	    create: true                    # missing folders are created on the nodes
//	  - path: /srv/shared/certs
//	    read_only: true                 # may only be mounted read-only
//	  - path: /mnt/nas/einsatz
//	    repos: [einsatz-app]            # only these repositories
//	  - path: /
//	    read_only: true
//	    repos: [observability]
//	    services: [node-exporter]       # only a service with this exact compose name
//
// Services narrows a rule below Repos: without it, every service of an
// allowed repo may use the rule, which is fine for an ordinary data folder
// but too wide for something like a read-only host root - grant that to one
// named service only, not to whatever else the repo's compose files define.
type BindRule struct {
	Path     string   `yaml:"path"`
	Create   bool     `yaml:"create"`
	ReadOnly bool     `yaml:"read_only"`
	Repos    []string `yaml:"repos"`
	Services []string `yaml:"services"`
}

// Bind is one bind mount of a rendered stack that passed the rules.
type Bind struct {
	Service     string // compose service name
	Source      string // cleaned host path
	ReadOnly    bool
	Root        string   // resolved rule path the source lives in
	Prefix      string   // static part of the rule (must exist on every node)
	Create      bool     // create missing folders
	Constraints []string // placement constraints of the service
}

// Root resolves the rule for a stack.
func (r BindRule) Root(repo, env, stack string) string {
	p := strings.NewReplacer("{repo}", spec.Slug(repo), "{env}", env, "{stack}", stack).Replace(r.Path)
	return path.Clean(p)
}

// Prefix is the part of the rule before the first placeholder. It must exist
// on every node; everything below it may be created by the controller.
func (r BindRule) Prefix() string {
	p := r.Path
	if i := strings.Index(p, "{"); i >= 0 {
		p = p[:i]
		if j := strings.LastIndex(p, "/"); j >= 0 {
			p = p[:j]
		}
	} else {
		p = path.Dir(path.Clean(p))
	}
	if p == "" {
		p = "/"
	}
	return path.Clean(p)
}

func (r BindRule) appliesTo(repo, service string) bool {
	if len(r.Repos) != 0 && !contains(r.Repos, repo) {
		return false
	}
	if len(r.Services) != 0 && !contains(r.Services, service) {
		return false
	}
	return true
}

// DataDir is the first creatable bind folder of a stack (exposed as ${STACK_DATA}).
// Rules scoped to specific services are skipped: STACK_DATA is a per-stack
// default, not tied to one service.
func (p *Policy) DataDir(repo, env, stack string) string {
	for _, r := range p.BindMounts {
		if r.Create && len(r.Services) == 0 && r.appliesTo(repo, "") {
			return r.Root(repo, env, stack)
		}
	}
	return ""
}

func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

func strictlyUnder(p, root string) bool { return p != root && under(p, root) }

func (c *checker) bind(service string, constraints []string, w, src string, readOnly bool) {
	if src == "" || !strings.HasPrefix(src, "/") {
		c.add(w, "bind mount source %q must be an absolute host path (use ${STACK_DATA}/...)", src)
		return
	}
	if c.ctx.WorkDir != "" && under(path.Clean(src), c.ctx.WorkDir) {
		c.add(w, "relative bind mounts are not supported, use ${STACK_DATA}/<folder>")
		return
	}
	clean := path.Clean(src)
	var best *BindRule
	var bestRoot string
	for i := range c.p.BindMounts {
		r := c.p.BindMounts[i]
		if !r.appliesTo(c.ctx.Repo, service) {
			continue
		}
		root := r.Root(c.ctx.Repo, c.ctx.Env, c.ctx.Stack)
		if under(clean, root) && len(root) > len(bestRoot) {
			best, bestRoot = &c.p.BindMounts[i], root
		}
	}
	if best == nil {
		var allowed []string
		for _, r := range c.p.BindMounts {
			if r.appliesTo(c.ctx.Repo, service) {
				allowed = append(allowed, r.Root(c.ctx.Repo, c.ctx.Env, c.ctx.Stack))
			}
		}
		if len(allowed) == 0 {
			c.add(w, "bind mount of host path %q is not allowed (no bind folders configured)", src)
		} else {
			c.add(w, "bind mount of host path %q is not allowed, use a folder below %s", src, strings.Join(allowed, " or "))
		}
		return
	}
	if best.ReadOnly && !readOnly {
		c.add(w, "%s may only be mounted read-only (add :ro / read_only: true)", bestRoot)
		return
	}
	c.binds = append(c.binds, Bind{
		Service: service, Source: clean, ReadOnly: readOnly, Root: bestRoot,
		Prefix: best.Prefix(), Create: best.Create, Constraints: constraints,
	})
}

// checkBindNesting rejects a writable bind mount that contains another bind
// source: the container could replace the inner folder with a symlink, and
// Docker follows symlinks when it mounts the inner one (host escape).
func (c *checker) checkBindNesting() {
	for _, outer := range c.binds {
		if outer.ReadOnly {
			continue
		}
		for _, inner := range c.binds {
			if strictlyUnder(inner.Source, outer.Source) {
				c.add("services."+inner.Service, "bind source %s lies inside %s, which %s mounts writable; nested bind mounts are not allowed", inner.Source, outer.Source, outer.Service)
			}
		}
	}
}

// CheckBindHistory rejects sources below a folder that was ever mounted
// writable by a container (keyed by rule root): a symlink could have been
// planted there. history maps root -> writable sources deployed before.
func CheckBindHistory(binds []Bind, history map[string][]string) []string {
	var v []string
	seen := map[string]bool{}
	for _, b := range binds {
		for _, h := range history[b.Root] {
			if strictlyUnder(b.Source, h) && !seen[b.Source] {
				seen[b.Source] = true
				v = append(v, fmt.Sprintf("services.%s: bind source %s lies inside %s, which was mounted writable before; a container could have placed a symlink there. An admin can clear this after checking the folder: swarm-gitops reset-binds %s", b.Service, b.Source, h, b.Root))
			}
		}
	}
	sort.Strings(v)
	return v
}

// WritableSources groups the writable bind sources of a stack by rule root.
func WritableSources(binds []Bind) map[string][]string {
	res := map[string][]string{}
	for _, b := range binds {
		if !b.ReadOnly && !contains(res[b.Root], b.Source) {
			res[b.Root] = append(res[b.Root], b.Source)
		}
	}
	return res
}
