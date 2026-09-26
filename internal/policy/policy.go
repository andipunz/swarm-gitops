// Package policy decides what a repo is allowed to deploy onto the Swarm.
// Checks run on the output of `docker stack config`, i.e. on exactly the
// merged and interpolated stack that is deployed afterwards.
package policy

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/andipunz/swarm-gitops/internal/spec"
	"gopkg.in/yaml.v3"
)

// ReservedLabelPrefix is owned by the controller; devs cannot set it.
const ReservedLabelPrefix = "swarm-gitops."

// Policy is loaded from the admin-controlled policy file.
type Policy struct {
	// Allowed image prefixes after normalisation, e.g. "ghcr.io/example/",
	// "docker.io/library/". Empty list allows every image.
	ImagePrefixes []string `yaml:"image_prefixes"`
	// External networks a stack may join, per environment; "*" applies to all.
	ExternalNetworks map[string][]string `yaml:"external_networks"`
	// Folders that may be bind-mounted. Empty = no bind mounts.
	BindMounts []BindRule `yaml:"bind_mounts"`
	// Traefik routing rules (hostnames per environment, reservations).
	Traefik TraefikPolicy `yaml:"traefik"`
	// Allow published ports for every repo (bypasses Traefik, port
	// conflicts) - keep false and use PublishedPorts for a narrow, explicit
	// exception instead (e.g. a log-shipping backend a node's own Docker
	// daemon needs to reach directly).
	AllowPublishedPorts bool `yaml:"allow_published_ports"`
	// Named exceptions to AllowPublishedPorts=false. Each entry names one
	// exact (port, mode) pair and the only repos allowed to publish it -
	// both Mode and Repos are required, so there's no "any mode"/"any repo"
	// wildcard to reach for by accident.
	PublishedPorts []PortRule `yaml:"published_ports"`
	// Capabilities that may be added.
	CapAdd []string `yaml:"cap_add"`
	// Allow plain Swarm secrets (external: true, created ahead of time with
	// `docker secret create`). No plugin needed, but not scoped by
	// swarm-gitops: an admin who names secrets predictably could let one
	// repo reference another's.
	AllowExternalSecrets bool `yaml:"allow_external_secrets"`
	// If set, secrets may also (or instead) use driver: <this value> - your
	// own secrets driver plugin, which can scope secrets per repo/env using
	// the labels swarm-gitops sets (see the README's "Secrets plugin" section).
	SecretDriver string `yaml:"secret_driver"`
	// Upper bound for deploy.replicas (0 = unlimited).
	MaxReplicas int `yaml:"max_replicas"`
	// Allowed "type" driver_opts for local volumes (network filesystems).
	VolumeMountTypes []string `yaml:"volume_mount_types"`
}

// Default is used when no policy file is configured.
func Default() *Policy {
	return &Policy{
		ExternalNetworks: map[string][]string{"*": {"traefik-public"}},
		MaxReplicas:      10,
		VolumeMountTypes: []string{"nfs", "nfs4", "cifs"},
	}
}

// Load reads a policy file; an empty path returns Default().
func Load(file string) (*Policy, error) {
	if file == "" {
		return Default(), nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("policy %s: %w", file, err)
	}
	for i, r := range p.PublishedPorts {
		if r.Mode != "host" && r.Mode != "ingress" {
			return nil, fmt.Errorf("policy %s: published_ports[%d]: mode must be \"host\" or \"ingress\"", file, i)
		}
		if len(r.Repos) == 0 {
			return nil, fmt.Errorf("policy %s: published_ports[%d]: repos is required (no org-wide port exceptions by accident)", file, i)
		}
	}
	for i, r := range p.BindMounts {
		if path.Clean(r.Path) == "/" && len(r.Services) == 0 {
			return nil, fmt.Errorf("policy %s: bind_mounts[%d]: path \"/\" also requires services (a host-root mount must be scoped to one named service, not every service the repo's compose files define)", file, i)
		}
	}
	return p, nil
}

// PortRule is one named exception to AllowPublishedPorts=false: repo may
// publish port Published in Mode, and nothing else may.
//
//	published_ports:
//	  - published: 3100
//	    mode: host
//	    repos: [observability]   # required: no wildcard
type PortRule struct {
	Published int      `yaml:"published"`
	Mode      string   `yaml:"mode"` // "host" or "ingress", required
	Repos     []string `yaml:"repos"`
}

// Context describes the stack being checked.
type Context struct {
	Stack   string
	Repo    string
	Env     string
	WorkDir string // absolute directory the .swarm files were materialised in
	// HostClaims maps hostnames already routed by Traefik to the stack (or
	// service) using them. nil skips the ownership check (e.g. in CI).
	HostClaims map[string]string
	// ObjectClaims maps a Traefik object key ("proto.kind.name", e.g.
	// "http.routers.app-prod") already declared by some service to the stack
	// (or service) using it. nil skips the ownership check (e.g. in CI).
	//
	// This exists on top of the "name must start with the stack name" rule
	// below: that rule alone doesn't guarantee stacks can't collide, because
	// one stack's name can be a hyphen-prefix of another's (e.g. "app-prod"
	// and "app-prod-x"), which would let both legitimately claim a name like
	// "app-prod-x-web". First-come-first-served ownership, checked the same
	// way as hostnames, closes that gap regardless of naming coincidences.
	ObjectClaims map[string]string
}

// Report is the result of a policy check.
type Report struct {
	Violations []string
	Binds      []Bind   // bind mounts, for folder preparation on the nodes
	Hosts      []string // hostnames this stack routes
	Objects    []string // Traefik object keys ("proto.kind.name") this stack declares
}

type checker struct {
	p       *Policy
	ctx     Context
	v       []string
	binds   []Bind
	hosts   map[string]bool
	objects map[string]bool
}

func (c *checker) add(where, format string, a ...any) {
	c.v = append(c.v, where+": "+fmt.Sprintf(format, a...))
}

// PrecheckRaw validates a single, not yet rendered compose file. It runs before
// `docker stack config`, which would otherwise read referenced files from the
// controller's filesystem (env_file: /run/secrets/... etc).
func PrecheckRaw(name string, data []byte) []string {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return []string{fmt.Sprintf("%s: invalid YAML: %v", name, err)}
	}
	c := &checker{}
	var walk func(where string, n any)
	walk = func(where string, n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				w := where + "." + k
				switch {
				case k == "labels" || k == "environment" || strings.HasPrefix(k, "x-"):
					continue
				case k == "include" || k == "extends":
					c.add(w, "%q is not supported", k)
					continue
				case k == "env_file" || k == "label_file" || k == "file" || k == "path":
					checkPaths(c, w, child)
				}
				walk(w, child)
			}
		case []any:
			for i, child := range v {
				walk(fmt.Sprintf("%s[%d]", where, i), child)
			}
		}
	}
	walk(name, doc)
	sort.Strings(c.v)
	return c.v
}

func checkPaths(c *checker, where string, n any) {
	switch v := n.(type) {
	case string:
		if !spec.SafeRelPath(v) {
			c.add(where, "path %q must be relative, without variables, and stay inside .swarm/", v)
		}
	case []any:
		for i, e := range v {
			checkPaths(c, fmt.Sprintf("%s[%d]", where, i), e)
		}
	case map[string]any:
		if p, ok := v["path"]; ok {
			checkPaths(c, where+".path", p)
		}
	}
}

// Check validates the rendered stack.
func (p *Policy) Check(stack map[string]any, ctx Context) Report {
	c := &checker{p: p, ctx: ctx, hosts: map[string]bool{}, objects: map[string]bool{}}
	for k := range stack {
		switch k {
		case "version", "services", "networks", "volumes", "configs", "secrets":
		default:
			if !strings.HasPrefix(k, "x-") {
				c.add(k, "top-level key not allowed")
			}
		}
	}
	services := asMap(stack["services"])
	if len(services) == 0 {
		c.add("services", "no services defined")
	}
	for _, name := range sortedKeys(services) {
		c.service(name, asMap(services[name]))
	}
	nets := asMap(stack["networks"])
	for _, name := range sortedKeys(nets) {
		c.network(name, asMap(nets[name]))
	}
	vols := asMap(stack["volumes"])
	for _, name := range sortedKeys(vols) {
		c.volume(name, asMap(vols[name]))
	}
	configs := asMap(stack["configs"])
	for _, name := range sortedKeys(configs) {
		c.config(name, asMap(configs[name]))
	}
	secrets := asMap(stack["secrets"])
	for _, name := range sortedKeys(secrets) {
		c.secret(name, asMap(secrets[name]))
	}
	c.checkBindNesting()
	r := Report{Violations: c.v, Binds: c.binds}
	for h := range c.hosts {
		r.Hosts = append(r.Hosts, h)
	}
	sort.Strings(r.Hosts)
	for o := range c.objects {
		r.Objects = append(r.Objects, o)
	}
	sort.Strings(r.Objects)
	return r
}

var deniedServiceKeys = map[string]string{
	"privileged":    "privileged containers are not allowed",
	"network_mode":  "network_mode is not allowed (use networks)",
	"pid":           "sharing the host PID namespace is not allowed",
	"ipc":           "sharing the host IPC namespace is not allowed",
	"userns_mode":   "userns_mode is not allowed",
	"devices":       "host devices are not allowed",
	"security_opt":  "security_opt is not allowed",
	"cgroup_parent": "cgroup_parent is not allowed",
	"cgroupns_mode": "cgroupns_mode is not allowed",
}

func (c *checker) service(name string, s map[string]any) {
	w := "services." + name
	if full := len(c.ctx.Stack) + 1 + len(name); full > 63 {
		c.add(w, "service name too long (%q is %d chars, Swarm allows 63)", c.ctx.Stack+"_"+name, full)
	}
	for _, k := range sortedKeys(s) {
		if msg, bad := deniedServiceKeys[k]; bad {
			if k == "privileged" && s[k] == false {
				continue
			}
			c.add(w+"."+k, "%s", msg)
		}
	}
	img, _ := s["image"].(string)
	if img == "" {
		c.add(w+".image", "image is required (images are not built on the Swarm)")
	} else if len(c.p.ImagePrefixes) > 0 {
		n := NormalizeImage(img)
		ok := false
		for _, pre := range c.p.ImagePrefixes {
			if strings.HasPrefix(n, pre) {
				ok = true
				break
			}
		}
		if !ok {
			c.add(w+".image", "image %q is not from an allowed registry (%s)", img, strings.Join(c.p.ImagePrefixes, ", "))
		}
	}
	for i, capName := range asList(s["cap_add"]) {
		cs, _ := capName.(string)
		if !contains(c.p.CapAdd, strings.TrimPrefix(strings.ToUpper(cs), "CAP_")) {
			c.add(fmt.Sprintf("%s.cap_add[%d]", w, i), "capability %q is not allowed", cs)
		}
	}
	deploy := asMap(s["deploy"])
	var constraints []string
	for _, cs := range asList(asMap(deploy["placement"])["constraints"]) {
		if str, ok := cs.(string); ok {
			constraints = append(constraints, str)
		}
	}
	sort.Strings(constraints)
	for i, v := range asList(s["volumes"]) {
		c.serviceVolume(name, constraints, fmt.Sprintf("%s.volumes[%d]", w, i), v)
	}
	if !c.p.AllowPublishedPorts {
		for i, port := range asList(s["ports"]) {
			if !c.p.portAllowed(c.ctx.Repo, port) {
				c.add(fmt.Sprintf("%s.ports[%d]", w, i), "published ports are not allowed, expose the service through Traefik labels (unless explicitly listed in the policy's published_ports)")
			}
		}
	}
	checkLabels(c, w+".labels", s["labels"])
	checkLabels(c, w+".deploy.labels", deploy["labels"])
	c.traefik(w, s["labels"], deploy["labels"])
	if c.p.MaxReplicas > 0 {
		if r, ok := toInt(deploy["replicas"]); ok && r > c.p.MaxReplicas {
			c.add(w+".deploy.replicas", "%d replicas exceed the limit of %d", r, c.p.MaxReplicas)
		}
	}
	for i, sref := range asList(s["secrets"]) {
		if m := asMap(sref); m != nil {
			if t, _ := m["target"].(string); strings.HasPrefix(t, "/") && !strings.HasPrefix(t, "/run/secrets/") {
				c.add(fmt.Sprintf("%s.secrets[%d].target", w, i), "secret target must be inside /run/secrets/")
			}
		}
	}
}

func (c *checker) serviceVolume(service string, constraints []string, w string, v any) {
	var typ, src string
	readOnly := false
	switch vv := v.(type) {
	case string:
		parts := strings.Split(vv, ":")
		if len(parts) == 1 {
			return // anonymous volume
		}
		src = parts[0]
		if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~") {
			typ = "bind"
		} else {
			typ = "volume"
		}
		if len(parts) > 2 {
			for _, o := range strings.Split(parts[2], ",") {
				readOnly = readOnly || o == "ro"
			}
		}
	case map[string]any:
		typ, _ = vv["type"].(string)
		src, _ = vv["source"].(string)
		readOnly = isTrue(vv["read_only"])
	}
	switch typ {
	case "volume", "tmpfs", "":
		return
	case "bind":
		c.bind(service, constraints, w, src, readOnly)
	default:
		c.add(w, "mount type %q is not allowed", typ)
	}
}

func (c *checker) network(name string, n map[string]any) {
	w := "networks." + name
	if isTrue(n["external"]) {
		real := name
		if nm, _ := n["name"].(string); nm != "" {
			real = nm
		}
		allowed := append(append([]string{}, c.p.ExternalNetworks["*"]...), c.p.ExternalNetworks[c.ctx.Env]...)
		if real == "host" || real == "bridge" || real == "ingress" || !contains(allowed, real) {
			c.add(w, "external network %q is not allowed in %s (allowed: %s)", real, c.ctx.Env, strings.Join(allowed, ", "))
		}
		return
	}
	if d, _ := n["driver"].(string); d != "" && d != "overlay" {
		c.add(w+".driver", "network driver %q is not allowed", d)
	}
	if _, ok := n["name"]; ok {
		c.add(w+".name", "custom network names are not allowed (the stack name is used as prefix)")
	}
}

func (c *checker) volume(name string, v map[string]any) {
	w := "volumes." + name
	if isTrue(v["external"]) {
		c.add(w, "external volumes are not allowed")
		return
	}
	if _, ok := v["name"]; ok {
		c.add(w+".name", "custom volume names are not allowed (the stack name is used as prefix)")
	}
	if d, _ := v["driver"].(string); d != "" && d != "local" {
		c.add(w+".driver", "volume driver %q is not allowed", d)
	}
	opts := asMap(v["driver_opts"])
	if len(opts) == 0 {
		return
	}
	t, _ := opts["type"].(string)
	o, _ := opts["o"].(string)
	if !contains(c.p.VolumeMountTypes, t) || strings.Contains(","+o+",", ",bind,") || strings.Contains(","+o+",", ",rbind,") {
		c.add(w+".driver_opts", "only network filesystem mounts (%s) are allowed as volume driver_opts", strings.Join(c.p.VolumeMountTypes, ", "))
	}
}

func (c *checker) config(name string, cfg map[string]any) {
	w := "configs." + name
	if isTrue(cfg["external"]) {
		c.add(w, "external configs are not allowed")
	}
	if _, ok := cfg["name"]; ok {
		c.add(w+".name", "custom config names are not allowed")
	}
	if f, ok := cfg["file"].(string); ok {
		if !within(c.ctx.WorkDir, f) {
			c.add(w+".file", "config file must be inside .swarm/")
		}
	}
}

// secret validates one top-level `secrets:` entry. Two ways to provide a
// secret are accepted, both opt-in via policy:
//   - external: true (+ optional name): a secret created ahead of time with
//     `docker secret create`, the standard Swarm way. It needs no plugin,
//     but isn't scoped by swarm-gitops: an admin who names secrets
//     predictably could let one repo reference another's, same as on any
//     plain Swarm today.
//   - driver: <secret_driver>: your own secrets driver plugin, which can
//     scope secrets per repo/env using the labels swarm-gitops sets on the
//     service and secret (see the README).
//
// `file:`/`environment:` are always rejected: the secret value would end up
// in git or in the rendered stack file.
func (c *checker) secret(name string, s map[string]any) {
	w := "secrets." + name
	for _, k := range []string{"file", "environment"} {
		if _, ok := s[k]; ok {
			c.add(w+"."+k, "%q is not allowed for secrets: the value would end up in git or the rendered stack", k)
		}
	}
	external := isTrue(s["external"])
	driver, hasDriver := s["driver"].(string)
	switch {
	case external:
		if !c.p.AllowExternalSecrets {
			c.add(w+".external", "external secrets are not allowed (%s)", c.secretHint())
		}
	case hasDriver && driver != "":
		if c.p.SecretDriver == "" || driver != c.p.SecretDriver {
			c.add(w+".driver", "driver %q is not allowed (%s)", driver, c.secretHint())
		}
	default:
		c.add(w, "secret needs %s", c.secretHint())
	}
	if _, ok := s["name"]; ok && !external {
		c.add(w+".name", "a custom secret name is only allowed together with external: true")
	}
}

// secretHint describes the way(s) this policy accepts secrets, for messages.
func (c *checker) secretHint() string {
	var opts []string
	if c.p.AllowExternalSecrets {
		opts = append(opts, "external: true")
	}
	if c.p.SecretDriver != "" {
		opts = append(opts, fmt.Sprintf("driver: %s", c.p.SecretDriver))
	}
	if len(opts) == 0 {
		return "this policy does not allow any secrets; set allow_external_secrets or secret_driver"
	}
	return strings.Join(opts, " or ")
}

func checkLabels(c *checker, w string, labels any) {
	for _, k := range labelKeys(labels) {
		if strings.HasPrefix(k, ReservedLabelPrefix) {
			c.add(w, "label %q is reserved for the controller", k)
		}
	}
}

func labelKeys(labels any) []string {
	var keys []string
	switch l := labels.(type) {
	case map[string]any:
		keys = sortedKeys(l)
	case map[string]string:
		keys = sortedStringKeys(l)
	case []any:
		for _, e := range l {
			if s, ok := e.(string); ok {
				k, _, _ := strings.Cut(s, "=")
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// NormalizeImage expands short names: nginx -> docker.io/library/nginx.
func NormalizeImage(img string) string {
	first, rest, hasSlash := strings.Cut(img, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return img
	}
	if !hasSlash {
		return "docker.io/library/" + img
	}
	_ = rest
	return "docker.io/" + img
}

func within(dir, file string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, filepath.Clean(file))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asList(v any) []any         { l, _ := v.([]any); return l }

func isTrue(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case map[string]any: // legacy "external: {name: x}"
		return true
	}
	return false
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case uint64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// portAllowed reports whether repo may publish the given rendered `ports:`
// entry under PublishedPorts. By the time policy.Check runs, `docker stack
// config` has already normalised every port to the long map form
// (mode/target/published/protocol) regardless of how the compose file wrote
// it, so that's the only shape this needs to parse.
func (p *Policy) portAllowed(repo string, port any) bool {
	m, ok := port.(map[string]any)
	if !ok {
		return false
	}
	mode, _ := m["mode"].(string)
	published, ok := toInt(m["published"])
	if !ok {
		return false
	}
	for _, r := range p.PublishedPorts {
		if r.Published == published && r.Mode == mode && contains(r.Repos, repo) {
			return true
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
