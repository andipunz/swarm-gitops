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

	"github.com/bergwacht-bayern/swarm-gitops/internal/spec"
	"gopkg.in/yaml.v3"
)

// ReservedLabelPrefix is owned by the controller; devs cannot set it.
const ReservedLabelPrefix = "swarm-gitops."

// Policy is loaded from the admin-controlled policy file.
type Policy struct {
	// Allowed image prefixes after normalisation, e.g. "ghcr.io/bergwacht-bayern/",
	// "docker.io/library/". Empty list allows every image.
	ImagePrefixes []string `yaml:"image_prefixes"`
	// External networks a stack may join, per environment; "*" applies to all.
	ExternalNetworks map[string][]string `yaml:"external_networks"`
	// Host path prefixes allowed as bind mount sources. Empty = no bind mounts.
	BindMounts []string `yaml:"bind_mounts"`
	// Allow published ports (ingress or host mode).
	AllowPublishedPorts bool `yaml:"allow_published_ports"`
	// Capabilities that may be added.
	CapAdd []string `yaml:"cap_add"`
	// Every secret must use this driver (your secrets plugin).
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
	return p, nil
}

// Context describes the stack being checked.
type Context struct {
	Stack   string
	Env     string
	WorkDir string // absolute directory the .swarm files were materialised in
}

type checker struct {
	p   *Policy
	ctx Context
	v   []string
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

// Check validates the rendered stack and returns human readable violations.
func (p *Policy) Check(stack map[string]any, ctx Context) []string {
	c := &checker{p: p, ctx: ctx}
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
	return c.v
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
	for i, v := range asList(s["volumes"]) {
		c.serviceVolume(fmt.Sprintf("%s.volumes[%d]", w, i), v)
	}
	if ports := asList(s["ports"]); len(ports) > 0 && !c.p.AllowPublishedPorts {
		c.add(w+".ports", "published ports are not allowed, expose the service through Traefik labels")
	}
	checkLabels(c, w+".labels", s["labels"])
	deploy := asMap(s["deploy"])
	checkLabels(c, w+".deploy.labels", deploy["labels"])
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

func (c *checker) serviceVolume(w string, v any) {
	var typ, src string
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
	case map[string]any:
		typ, _ = vv["type"].(string)
		src, _ = vv["source"].(string)
	}
	switch typ {
	case "volume", "tmpfs", "":
		return
	case "bind":
		clean := path.Clean(src)
		for _, allowed := range c.p.BindMounts {
			a := path.Clean(allowed)
			if clean == a || strings.HasPrefix(clean, strings.TrimSuffix(a, "/")+"/") {
				return
			}
		}
		c.add(w, "bind mount of host path %q is not allowed", src)
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

func (c *checker) secret(name string, s map[string]any) {
	w := "secrets." + name
	for _, k := range []string{"file", "environment", "external", "name"} {
		if _, ok := s[k]; ok {
			c.add(w+"."+k, "%q is not allowed for secrets, use driver: %s", k, c.p.SecretDriver)
		}
	}
	if c.p.SecretDriver != "" {
		if d, _ := s["driver"].(string); d != c.p.SecretDriver {
			c.add(w+".driver", "secrets must use driver %q", c.p.SecretDriver)
		}
	}
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
