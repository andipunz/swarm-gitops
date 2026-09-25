package policy

import (
	"fmt"
	"sort"
	"strings"
)

// TraefikPolicy controls which hostnames a stack may route.
//
//	traefik:
//	  hosts:                        # allowed hostnames per environment ("*" = all)
//	    prod: [bergwacht-bayern.de, "*.bergwacht-bayern.de"]
//	    sandbox: ["*.sandbox.bergwacht-bayern.de"]
//	  reserved:                     # hostname (or *.pattern) -> only this repository
//	    einsatz.bergwacht-bayern.de: einsatz-app
//	  entrypoints: [websecure]      # optional allowlist
//
// Independently of this, a hostname belongs to the stack that routes it
// first; other stacks can't use it.
type TraefikPolicy struct {
	Hosts       map[string][]string `yaml:"hosts"`
	Reserved    map[string]string   `yaml:"reserved"`
	Entrypoints []string            `yaml:"entrypoints"`
}

// Matcher is one call in a Traefik rule, e.g. Host(`a`, `b`).
type Matcher struct {
	Name string
	Args []string
}

// ParseRule parses a Traefik rule that only uses && between matchers.
// || and ! are rejected: with them a rule can match hosts it doesn't name
// (e.g. Host(`mine`) || PathPrefix(`/`) catches every host).
func ParseRule(rule string) ([]Matcher, error) {
	var ms []Matcher
	depth := 0
	i := 0
	n := len(rule)
	skip := func() {
		for i < n && (rule[i] == ' ' || rule[i] == '\t' || rule[i] == '\n') {
			i++
		}
	}
	readString := func() (string, error) {
		q := rule[i]
		i++
		var b strings.Builder
		for i < n && rule[i] != q {
			if q == '"' && rule[i] == '\\' && i+1 < n {
				i++
			}
			b.WriteByte(rule[i])
			i++
		}
		if i >= n {
			return "", fmt.Errorf("unterminated string")
		}
		i++
		return b.String(), nil
	}
	expectOperand := true
	for {
		skip()
		if i >= n {
			break
		}
		ch := rule[i]
		switch {
		case ch == '(' && expectOperand:
			depth++
			i++
		case ch == ')' && !expectOperand:
			if depth == 0 {
				return nil, fmt.Errorf("unbalanced parentheses")
			}
			depth--
			i++
		case strings.HasPrefix(rule[i:], "&&") && !expectOperand:
			i += 2
			expectOperand = true
		case strings.HasPrefix(rule[i:], "||"):
			return nil, fmt.Errorf("'||' is not allowed, use one router per host rule")
		case ch == '!':
			return nil, fmt.Errorf("'!' (negation) is not allowed")
		case expectOperand && (ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'):
			start := i
			for i < n && (rule[i] >= 'A' && rule[i] <= 'Z' || rule[i] >= 'a' && rule[i] <= 'z' || rule[i] >= '0' && rule[i] <= '9') {
				i++
			}
			m := Matcher{Name: rule[start:i]}
			skip()
			if i >= n || rule[i] != '(' {
				return nil, fmt.Errorf("expected '(' after %s", m.Name)
			}
			i++
			for {
				skip()
				if i < n && rule[i] == ')' {
					i++
					break
				}
				if i >= n || (rule[i] != '`' && rule[i] != '"') {
					return nil, fmt.Errorf("%s: arguments must be quoted strings", m.Name)
				}
				s, err := readString()
				if err != nil {
					return nil, err
				}
				m.Args = append(m.Args, s)
				skip()
				if i < n && rule[i] == ',' {
					i++
				}
			}
			ms = append(ms, m)
			expectOperand = false
		default:
			return nil, fmt.Errorf("unexpected %q at position %d", string(ch), i)
		}
	}
	if depth != 0 || expectOperand && len(ms) > 0 {
		return nil, fmt.Errorf("incomplete rule")
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("empty rule")
	}
	return ms, nil
}

// HostsFromLabels returns the hostnames routed by a service's labels
// (best effort, used to find which hosts are already taken on the Swarm).
func HostsFromLabels(labels map[string]string) []string {
	var hosts []string
	for k, v := range labels {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "traefik.") || !strings.HasSuffix(lk, ".rule") {
			continue
		}
		ms, err := ParseRule(v)
		if err != nil {
			// A rule we can't parse (e.g. with ||) still claims the literal hosts in it.
			ms = looseMatchers(v)
		}
		for _, m := range ms {
			if strings.EqualFold(m.Name, "Host") || strings.EqualFold(m.Name, "HostSNI") || strings.EqualFold(m.Name, "HostHeader") {
				for _, h := range m.Args {
					hosts = append(hosts, strings.ToLower(strings.TrimSpace(h)))
				}
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

func looseMatchers(rule string) []Matcher {
	var ms []Matcher
	for _, name := range []string{"Host(", "HostSNI(", "HostHeader("} {
		rest := rule
		for {
			i := strings.Index(rest, name)
			if i < 0 {
				break
			}
			rest = rest[i+len(name):]
			end := strings.Index(rest, ")")
			if end < 0 {
				break
			}
			m := Matcher{Name: strings.TrimSuffix(name, "(")}
			for _, a := range strings.Split(rest[:end], ",") {
				m.Args = append(m.Args, strings.Trim(strings.TrimSpace(a), "`\""))
			}
			ms = append(ms, m)
		}
	}
	return ms
}

type router struct {
	proto string // http, tcp
	attrs map[string]string
}

func (c *checker) traefik(w string, containerLabels, deployLabels any) {
	for _, k := range labelKeys(containerLabels) {
		if strings.HasPrefix(strings.ToLower(k), "traefik.") {
			c.add(w+".labels", "Traefik labels belong under deploy.labels (%s)", k)
		}
	}
	labels := labelStrings(deployLabels)
	enabled := strings.EqualFold(labels["traefik.enable"], "true")
	routers := map[string]*router{}
	for _, k := range sortedStringKeys(labels) {
		lk := strings.ToLower(k)
		parts := strings.SplitN(lk, ".", 5)
		if len(parts) < 4 || parts[0] != "traefik" {
			continue
		}
		proto, kind, name := parts[1], parts[2], parts[3]
		if proto != "http" && proto != "tcp" && proto != "udp" {
			continue
		}
		switch kind {
		case "routers", "services", "middlewares", "serverstransports":
		default:
			continue
		}
		if !c.ownName(name) {
			c.add(w+".deploy.labels", "Traefik %s name %q must start with the stack name (use %s-<name> or ${STACK})", strings.TrimSuffix(kind, "s"), name, c.ctx.Stack)
			continue
		}
		if kind != "routers" {
			continue
		}
		if proto == "udp" {
			c.add(w+".deploy.labels", "UDP routers are not allowed")
			continue
		}
		r := routers[name]
		if r == nil {
			r = &router{proto: proto, attrs: map[string]string{}}
			routers[name] = r
		}
		if len(parts) == 5 {
			r.attrs[parts[4]] = labels[k]
		}
	}
	if enabled && len(routers) == 0 {
		c.add(w+".deploy.labels", "traefik.enable is set but no router is defined; define traefik.http.routers.${STACK}.rule with a Host() rule")
	}
	for _, name := range sortedStringKeys(routers) {
		c.router(w, name, routers[name])
	}
}

func (c *checker) ownName(name string) bool {
	s := strings.ToLower(c.ctx.Stack)
	return name == s || strings.HasPrefix(name, s+"-") || strings.HasPrefix(name, s+"_")
}

func (c *checker) router(w, name string, r *router) {
	where := fmt.Sprintf("%s.deploy.labels (router %s)", w, name)
	rule, ok := r.attrs["rule"]
	if !ok {
		c.add(where, "router needs an explicit rule with Host()")
		return
	}
	ms, err := ParseRule(rule)
	if err != nil {
		c.add(where, "rule %q: %v", rule, err)
		return
	}
	hostMatcher := "host"
	if r.proto == "tcp" {
		hostMatcher = "hostsni"
	}
	var hosts []string
	for _, m := range ms {
		switch strings.ToLower(m.Name) {
		case "hostregexp", "hostsniregexp":
			c.add(where, "%s is not allowed, list hostnames with %s()", m.Name, map[string]string{"host": "Host", "hostsni": "HostSNI"}[hostMatcher])
			return
		case hostMatcher, "hostheader":
			hosts = append(hosts, m.Args...)
		}
	}
	if len(hosts) == 0 {
		c.add(where, "rule %q must contain a Host() matcher", rule)
		return
	}
	for _, h := range hosts {
		c.host(where, strings.ToLower(strings.TrimSpace(h)))
	}
	for key, val := range r.attrs {
		if strings.HasPrefix(key, "tls.domains[") && (strings.HasSuffix(key, ".main") || strings.HasSuffix(key, ".sans")) {
			for _, d := range strings.Split(val, ",") {
				d = strings.ToLower(strings.TrimSpace(d))
				if d != "" && !c.hostAllowed(strings.TrimPrefix(d, "*.")) {
					c.add(where, "TLS domain %q is not allowed in %s", d, c.ctx.Env)
				}
			}
		}
	}
	if eps, ok := r.attrs["entrypoints"]; ok && len(c.p.Traefik.Entrypoints) > 0 {
		for _, ep := range strings.Split(eps, ",") {
			if ep = strings.TrimSpace(ep); !contains(c.p.Traefik.Entrypoints, ep) {
				c.add(where, "entrypoint %q is not allowed (allowed: %s)", ep, strings.Join(c.p.Traefik.Entrypoints, ", "))
			}
		}
	}
	if svc, ok := r.attrs["service"]; ok {
		base, provider, _ := strings.Cut(strings.ToLower(svc), "@")
		switch {
		case svc == "noop@internal":
		case provider != "" && provider != "swarm" && provider != "docker":
			c.add(where, "router service %q is not allowed (only services of this stack)", svc)
		case !c.ownName(base):
			c.add(where, "router service %q belongs to another stack", svc)
		}
	}
}

func (c *checker) host(where, h string) {
	if h == "" || strings.ContainsAny(h, "*{}/ ") {
		c.add(where, "invalid hostname %q", h)
		return
	}
	if !c.hostAllowed(h) {
		var allowed []string
		allowed = append(allowed, c.p.Traefik.Hosts["*"]...)
		allowed = append(allowed, c.p.Traefik.Hosts[c.ctx.Env]...)
		c.add(where, "hostname %q is not allowed in %s (allowed: %s)", h, c.ctx.Env, strings.Join(allowed, ", "))
		return
	}
	for _, pattern := range sortedStringKeys(c.p.Traefik.Reserved) {
		owner := c.p.Traefik.Reserved[pattern]
		if matchHost(h, strings.ToLower(pattern)) && owner != c.ctx.Repo {
			c.add(where, "hostname %q is reserved for repository %s", h, owner)
			return
		}
	}
	if owner, taken := c.ctx.HostClaims[h]; taken && owner != c.ctx.Stack {
		c.add(where, "hostname %q is already used by %s", h, owner)
		return
	}
	c.hosts[h] = true
}

func (c *checker) hostAllowed(h string) bool {
	patterns := append(append([]string{}, c.p.Traefik.Hosts["*"]...), c.p.Traefik.Hosts[c.ctx.Env]...)
	if len(c.p.Traefik.Hosts) == 0 {
		return true
	}
	for _, p := range patterns {
		if matchHost(h, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// matchHost: "example.org" matches exactly, "*.example.org" matches any subdomain.
func matchHost(h, pattern string) bool {
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(h, pattern[1:]) && len(h) > len(pattern)-1
	}
	return h == pattern
}

func labelStrings(labels any) map[string]string {
	res := map[string]string{}
	switch l := labels.(type) {
	case map[string]any:
		for k, v := range l {
			res[k] = fmt.Sprint(v)
		}
	case map[string]string:
		for k, v := range l {
			res[k] = v
		}
	case []any:
		for _, e := range l {
			if s, ok := e.(string); ok {
				k, v, _ := strings.Cut(s, "=")
				res[k] = v
			}
		}
	}
	return res
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
