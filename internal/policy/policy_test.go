package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRule(t *testing.T) {
	ok := map[string][]string{
		"Host(`a.example.org`)":                                  {"a.example.org"},
		"Host(`a.example.org`) && PathPrefix(`/api`)":            {"a.example.org"},
		"(Host(`a.example.org`) && (Path(`/x`)))":                {"a.example.org"},
		"Host(`a.example.org`, `b.example.org`)":                 {"a.example.org", "b.example.org"},
		`Host("a.example.org") && Headers("X-Foo", "b")`:         {"a.example.org"},
		"PathPrefix(`/api`) && Host(`a.example.org`)":            {"a.example.org"},
		"Host(`a.example.org`) && ClientIP(`10.0.0.0/8`)":        {"a.example.org"},
		"  Host( `a.example.org` )  &&  Method(`GET`)  ":         {"a.example.org"},
		"HostSNI(`db.example.org`)":                              {"db.example.org"},
		"Host(`a.example.org`) && Query(`x`, `a&&b||c!`)":        {"a.example.org"},
		"Host(`a.example.org`) && PathRegexp(`^/(a|b)$`)":        {"a.example.org"},
		"Host(`A.Example.org`)":                                  {"A.Example.org"},
		"Host(`a.example.org`)&&Path(`/`)":                       {"a.example.org"},
		"Host(\"a\\\"b\")":                                       {`a"b`},
		"Host(`a.example.org`) && !Path(`/admin`)":               nil, // negation rejected
		"Host(`a.example.org`) || PathPrefix(`/`)":               nil,
		"Host(`a.example.org`) && Path(`/x`) || Path(`/y`)":      nil,
		"Host(`a.example.org`":                                   nil,
		"Host(a.example.org)":                                    nil,
		"":                                                       nil,
		"Host(`a.example.org`) &&":                               nil,
		"Host(`a.example.org`)) && (Path(`/`)":                   nil,
		"Host(`a.example.org`) Path(`/`)":                        nil,
		"Host(`a.example.org`) & Path(`/`)":                      nil,
		"Host(`a.example.org`) && Path(`/`) && HostRegexp(`.*`)": {"a.example.org"}, // parsed; rejected by the router check
	}
	for rule, wantHosts := range ok {
		ms, err := ParseRule(rule)
		if wantHosts == nil {
			if err == nil {
				t.Errorf("ParseRule(%q) should fail", rule)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRule(%q): %v", rule, err)
			continue
		}
		var hosts []string
		for _, m := range ms {
			if m.Name == "Host" || m.Name == "HostSNI" {
				hosts = append(hosts, m.Args...)
			}
		}
		if strings.Join(hosts, ",") != strings.Join(wantHosts, ",") {
			t.Errorf("ParseRule(%q) hosts = %v, want %v", rule, hosts, wantHosts)
		}
	}
}

func testPolicy() *Policy {
	p := Default()
	p.Traefik = TraefikPolicy{
		Hosts: map[string][]string{
			"prod":    {"example.com", "*.example.com"},
			"sandbox": {"*.sandbox.example.com"},
		},
		Reserved:    map[string]string{"einsatz.example.com": "einsatz-app", "*.intern.example.com": "intranet"},
		Entrypoints: []string{"websecure"},
	}
	p.BindMounts = []BindRule{
		{Path: "/srv/swarm/{repo}/{env}", Create: true},
		{Path: "/srv/shared/certs", ReadOnly: true},
		{Path: "/mnt/nas/einsatz", Repos: []string{"einsatz-app"}},
	}
	return p
}

func stack(labels map[string]any, volumes ...any) map[string]any {
	svc := map[string]any{"image": "ghcr.io/example/app:1", "deploy": map[string]any{"labels": labels}}
	if len(volumes) > 0 {
		svc["volumes"] = volumes
	}
	return map[string]any{"services": map[string]any{"app": svc}}
}

func check(p *Policy, doc map[string]any, repo, env string, claims map[string]string) Report {
	st := repo + "-" + env
	return p.Check(doc, Context{Stack: st, Repo: repo, Env: env, WorkDir: "/data/work/" + st + "/c1", HostClaims: claims})
}

func TestTraefik(t *testing.T) {
	p := testPolicy()
	cases := []struct {
		name, repo, env string
		labels          map[string]any
		claims          map[string]string
		want            string // "" = ok
	}{
		{"ok", "app", "prod", map[string]any{"traefik.enable": "true", "traefik.http.routers.app-prod.rule": "Host(`app.example.com`)", "traefik.http.routers.app-prod.entrypoints": "websecure"}, nil, ""},
		{"ok with suffix name", "app", "prod", map[string]any{"traefik.http.routers.app-prod-api.rule": "Host(`api.example.com`) && PathPrefix(`/v1`)"}, nil, ""},
		{"foreign domain", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`evil.com`)"}, nil, "not allowed in prod"},
		{"sandbox using prod host", "app", "sandbox", map[string]any{"traefik.http.routers.app-sandbox.rule": "Host(`app.example.com`)"}, nil, "not allowed in sandbox"},
		{"taken by other stack", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`www.example.com`)"}, map[string]string{"www.example.com": "stack website-prod"}, "already used by stack website-prod"},
		{"own claim is fine", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`app.example.com`)"}, map[string]string{"app.example.com": "app-prod"}, ""},
		{"reserved exact", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`einsatz.example.com`)"}, nil, "reserved for repository einsatz-app"},
		{"reserved owner ok", "einsatz-app", "prod", map[string]any{"traefik.http.routers.einsatz-app-prod.rule": "Host(`einsatz.example.com`)"}, nil, ""},
		{"reserved wildcard", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`wiki.intern.example.com`)"}, nil, "reserved for repository intranet"},
		{"catch-all or", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`app.example.com`) || PathPrefix(`/`)"}, nil, "'||' is not allowed"},
		{"no host", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "PathPrefix(`/`)"}, nil, "must contain a Host() matcher"},
		{"regexp", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "HostRegexp(`.+`)"}, nil, "HostRegexp is not allowed"},
		{"router name of other stack", "app", "prod", map[string]any{"traefik.http.routers.website-prod.rule": "Host(`app.example.com`)"}, nil, "must start with the stack name"},
		{"middleware name of other stack", "app", "prod", map[string]any{"traefik.http.middlewares.auth.basicauth.users": "x"}, nil, "must start with the stack name"},
		{"enable without router", "app", "prod", map[string]any{"traefik.enable": "true", "traefik.http.services.app-prod.loadbalancer.server.port": "80"}, nil, "no router is defined"},
		{"router without rule", "app", "prod", map[string]any{"traefik.http.routers.app-prod.entrypoints": "websecure"}, nil, "explicit rule"},
		{"dashboard service", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`app.example.com`)", "traefik.http.routers.app-prod.service": "api@internal"}, nil, "not allowed (only services of this stack)"},
		{"other stack's service", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`app.example.com`)", "traefik.http.routers.app-prod.service": "website-prod@swarm"}, nil, "belongs to another stack"},
		{"entrypoint", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`app.example.com`)", "traefik.http.routers.app-prod.entrypoints": "traefik"}, nil, "entrypoint \"traefik\""},
		{"tls domain", "app", "prod", map[string]any{"traefik.http.routers.app-prod.rule": "Host(`app.example.com`)", "traefik.http.routers.app-prod.tls.domains[0].main": "google.com"}, nil, "TLS domain"},
		{"udp", "app", "prod", map[string]any{"traefik.udp.routers.app-prod.entrypoints": "dns"}, nil, "UDP routers"},
		{"tcp catch all", "app", "prod", map[string]any{"traefik.tcp.routers.app-prod.rule": "HostSNI(`*`)"}, nil, "invalid hostname"},
		{"case-insensitive keys", "app", "prod", map[string]any{"Traefik.HTTP.Routers.Other.Rule": "Host(`app.example.com`)"}, nil, "must start with the stack name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := check(p, stack(tc.labels), tc.repo, tc.env, tc.claims)
			got := strings.Join(r.Violations, "\n")
			if tc.want == "" && got != "" {
				t.Fatalf("unexpected violations:\n%s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got:\n%s", tc.want, got)
			}
		})
	}
}

func TestPublishedPorts(t *testing.T) {
	port := func(published int, mode string) map[string]any {
		return map[string]any{"mode": mode, "target": published, "published": published, "protocol": "tcp"}
	}
	doc := func(p any) map[string]any {
		return map[string]any{"services": map[string]any{"app": map[string]any{
			"image": "ghcr.io/example/app:1", "ports": []any{p},
		}}}
	}
	rule := PortRule{Published: 3100, Mode: "host", Repos: []string{"observability"}}
	ctx := func(repo string) Context {
		return Context{Stack: repo + "-prod", Repo: repo, Env: "prod", WorkDir: "/data/work/" + repo + "-prod/c1"}
	}

	cases := []struct {
		name   string
		policy Policy
		repo   string
		port   map[string]any
		want   string // "" = ok
	}{
		{"denied by default", Policy{}, "observability", port(3100, "host"), "published ports are not allowed"},
		{"allowed: exact repo/port/mode match", Policy{PublishedPorts: []PortRule{rule}}, "observability", port(3100, "host"), ""},
		{"wrong repo denied", Policy{PublishedPorts: []PortRule{rule}}, "some-other-repo", port(3100, "host"), "published ports are not allowed"},
		{"wrong port denied", Policy{PublishedPorts: []PortRule{rule}}, "observability", port(3101, "host"), "published ports are not allowed"},
		{"wrong mode denied", Policy{PublishedPorts: []PortRule{rule}}, "observability", port(3100, "ingress"), "published ports are not allowed"},
		{"AllowPublishedPorts bypasses everything", Policy{AllowPublishedPorts: true}, "anyone", port(9999, "ingress"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.policy
			r := p.Check(doc(tc.port), ctx(tc.repo))
			got := strings.Join(r.Violations, "\n")
			if tc.want == "" && got != "" {
				t.Fatalf("unexpected violations:\n%s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got:\n%s", tc.want, got)
			}
		})
	}
}

func TestPublishedPortsLoadValidation(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		f := filepath.Join(t.TempDir(), "policy.yml")
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}

	t.Run("missing mode rejected", func(t *testing.T) {
		f := write(t, "published_ports:\n  - published: 3100\n    repos: [observability]\n")
		if _, err := Load(f); err == nil || !strings.Contains(err.Error(), "mode must be") {
			t.Fatalf("want a mode error, got %v", err)
		}
	})

	t.Run("missing repos rejected", func(t *testing.T) {
		f := write(t, "published_ports:\n  - published: 3100\n    mode: host\n")
		if _, err := Load(f); err == nil || !strings.Contains(err.Error(), "repos is required") {
			t.Fatalf("want a repos error, got %v", err)
		}
	})

	t.Run("valid rule accepted", func(t *testing.T) {
		f := write(t, "published_ports:\n  - published: 3100\n    mode: host\n    repos: [observability]\n")
		p, err := Load(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.PublishedPorts) != 1 || p.PublishedPorts[0].Published != 3100 {
			t.Fatalf("got %+v", p.PublishedPorts)
		}
	})
}

func TestSecrets(t *testing.T) {
	doc := func(sec map[string]any) map[string]any {
		return map[string]any{
			"services": map[string]any{"app": map[string]any{"image": "ghcr.io/example/app:1", "secrets": []any{"db"}}},
			"secrets":  map[string]any{"db": sec},
		}
	}
	ctx := Context{Stack: "app-prod", Repo: "app", Env: "prod", WorkDir: "/data/work/app-prod/c1"}
	withDriver := func(name string) Policy { p := *Default(); p.SecretDriver = name; return p }
	withExternal := func() Policy { p := *Default(); p.AllowExternalSecrets = true; return p }

	cases := []struct {
		name   string
		policy Policy
		secret map[string]any
		want   string // "" = ok
	}{
		{"nothing configured", *Default(), map[string]any{"driver": "x"}, "does not allow any secrets"},
		{"driver mismatch", withDriver("acme/secrets"), map[string]any{"driver": "other"}, "not allowed"},
		{"driver ok", withDriver("acme/secrets"), map[string]any{"driver": "acme/secrets"}, ""},
		{"external blocked by default", *Default(), map[string]any{"external": true}, "external secrets are not allowed"},
		{"external allowed", withExternal(), map[string]any{"external": true}, ""},
		{"external with name allowed", withExternal(), map[string]any{"external": true, "name": "shared_db_password"}, ""},
		{"custom name without external rejected", withDriver("acme/secrets"), map[string]any{"driver": "acme/secrets", "name": "x"}, "only allowed together with external"},
		{"explicit external:false with disallowed driver is still checked", withExternal(), map[string]any{"external": false, "driver": "attacker/secrets"}, "not allowed"},
		{"file rejected even with driver configured", withDriver("acme/secrets"), map[string]any{"driver": "acme/secrets", "file": "./s.txt"}, "not allowed for secrets"},
		{"environment rejected even when external allowed", withExternal(), map[string]any{"external": true, "environment": "SECRET"}, "not allowed for secrets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.policy
			r := p.Check(doc(tc.secret), ctx)
			got := strings.Join(r.Violations, "\n")
			if tc.want == "" && got != "" {
				t.Fatalf("unexpected violations:\n%s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got:\n%s", tc.want, got)
			}
		})
	}
}

// TestTraefikObjectOwnership covers the gap the name-prefix rule alone
// leaves open: stack "app-prod-x" is a hyphen-extension of stack "app-prod",
// so a router named "app-prod-x-web" satisfies BOTH stacks' own "must start
// with the stack name" check. Only claim-based ownership (like hostnames)
// stops "app-prod-x" from colliding with a name "app-prod" already uses.
// TestTraefikBadNameReportedOnce covers the other half of the same dedup fix:
// a name-prefix violation must also be reported once per object, not once
// per label naming it.
func TestTraefikBadNameReportedOnce(t *testing.T) {
	p := testPolicy()
	labels := map[string]any{
		"traefik.http.routers.website-prod.rule":        "Host(`app.example.com`)",
		"traefik.http.routers.website-prod.entrypoints": "websecure",
	}
	r := check(p, stack(labels), "app", "prod", nil)
	if len(r.Violations) != 1 {
		t.Fatalf("want exactly one violation (not one per label), got %d:\n%s", len(r.Violations), strings.Join(r.Violations, "\n"))
	}
}

func TestTraefikObjectOwnership(t *testing.T) {
	p := testPolicy()
	// Several labels name the same router: the conflict must be reported once,
	// not once per label.
	labels := map[string]any{
		"traefik.http.routers.app-prod-x-web.rule":        "Host(`app.example.com`)",
		"traefik.http.routers.app-prod-x-web.entrypoints": "websecure",
	}
	ctx := Context{Stack: "app-prod-x", Repo: "app-prod-x", Env: "prod", WorkDir: "/data/work/app-prod-x/c1"}

	ctx.ObjectClaims = map[string]string{"http.routers.app-prod-x-web": "stack app-prod"}
	r := p.Check(stack(labels), ctx)
	got := strings.Join(r.Violations, "\n")
	if !strings.Contains(got, "already used by stack app-prod") {
		t.Fatalf("want ownership violation, got:\n%s", got)
	}
	if len(r.Violations) != 1 {
		t.Fatalf("want exactly one violation (not one per label), got %d:\n%s", len(r.Violations), got)
	}

	ctx.ObjectClaims = nil
	r = p.Check(stack(labels), ctx)
	if len(r.Violations) != 0 {
		t.Fatalf("unexpected violations: %v", r.Violations)
	}
	if strings.Join(r.Objects, ",") != "http.routers.app-prod-x-web" {
		t.Fatalf("objects = %v", r.Objects)
	}
}

func TestTraefikLabelsOnContainer(t *testing.T) {
	doc := stack(nil)
	doc["services"].(map[string]any)["app"].(map[string]any)["labels"] = map[string]any{"traefik.http.routers.app-prod.rule": "Host(`x`)"}
	r := check(testPolicy(), doc, "app", "prod", nil)
	if !strings.Contains(strings.Join(r.Violations, "\n"), "belong under deploy.labels") {
		t.Fatal(r.Violations)
	}
}

func TestHostsFromLabels(t *testing.T) {
	h := HostsFromLabels(map[string]string{
		"traefik.http.routers.a.rule":   "Host(`A.example.org`) && Path(`/`)",
		"traefik.http.routers.b.rule":   "Host(`b.example.org`) || Host(`c.example.org`)", // legacy stacks may use ||
		"traefik.tcp.routers.db.rule":   "HostSNI(`db.example.org`)",
		"traefik.http.routers.a.middle": "Host(`ignored`)",
	})
	if strings.Join(h, ",") != "a.example.org,b.example.org,c.example.org,db.example.org" {
		t.Fatal(h)
	}
}

func TestBinds(t *testing.T) {
	p := testPolicy()
	bind := func(src string, ro bool) map[string]any {
		return map[string]any{"type": "bind", "source": src, "target": "/x", "read_only": ro}
	}
	cases := []struct {
		name, repo string
		vols       []any
		want       string
		wantBinds  int
	}{
		{"own folder", "app", []any{bind("/srv/swarm/app/prod/uploads", false)}, "", 1},
		{"own root", "app", []any{bind("/srv/swarm/app/prod", false)}, "", 1},
		{"short syntax", "app", []any{"/srv/swarm/app/prod/db:/var/lib/db"}, "", 1},
		{"other repo's folder", "app", []any{bind("/srv/swarm/website/prod", false)}, "not allowed, use a folder below /srv/swarm/app/prod", 0},
		{"other env's folder", "app", []any{bind("/srv/swarm/app/staging", false)}, "not allowed", 0},
		{"escape with ..", "app", []any{bind("/srv/swarm/app/prod/../../website/prod", false)}, "not allowed", 0},
		{"docker socket", "app", []any{"/var/run/docker.sock:/var/run/docker.sock"}, "not allowed", 0},
		{"shared read-only ok", "app", []any{bind("/srv/shared/certs", true)}, "", 1},
		{"shared read-only short", "app", []any{"/srv/shared/certs/x:/certs:ro"}, "", 1},
		{"shared writable", "app", []any{bind("/srv/shared/certs", false)}, "may only be mounted read-only", 0},
		{"repo-restricted rule, other repo", "app", []any{bind("/mnt/nas/einsatz", false)}, "not allowed", 0},
		{"repo-restricted rule, owner", "einsatz-app", []any{bind("/mnt/nas/einsatz/files", false)}, "", 1},
		{"relative", "app", []any{bind("/data/work/app-prod/c1/data", false)}, "relative bind mounts are not supported", 0},
		{"nested writable", "app", []any{bind("/srv/swarm/app/prod", false), bind("/srv/swarm/app/prod/uploads", false)}, "nested bind mounts are not allowed", 2},
		{"nested under read-only is ok", "app", []any{bind("/srv/swarm/app/prod", true), bind("/srv/swarm/app/prod/uploads", false)}, "", 2},
		{"same folder twice", "app", []any{bind("/srv/swarm/app/prod/a", false), bind("/srv/swarm/app/prod/a", false)}, "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := check(p, stack(nil, tc.vols...), tc.repo, "prod", nil)
			got := strings.Join(r.Violations, "\n")
			if tc.want == "" && got != "" {
				t.Fatalf("unexpected violations:\n%s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got:\n%s", tc.want, got)
			}
			if len(r.Binds) != tc.wantBinds {
				t.Fatalf("binds = %+v", r.Binds)
			}
		})
	}
	r := check(p, stack(nil, bind("/srv/swarm/app/prod/uploads", false)), "app", "prod", nil)
	b := r.Binds[0]
	if b.Root != "/srv/swarm/app/prod" || b.Prefix != "/srv/swarm" || !b.Create {
		t.Fatalf("%+v", b)
	}
	if d := p.DataDir("App_X", "prod", "app-x-prod"); d != "/srv/swarm/app-x/prod" {
		t.Fatal(d)
	}
}

func TestBindServiceScoping(t *testing.T) {
	p := Default()
	p.BindMounts = []BindRule{
		{Path: "/", ReadOnly: true, Repos: []string{"observability"}, Services: []string{"node-exporter"}},
	}
	doc := func(service, src string, ro bool) map[string]any {
		return map[string]any{"services": map[string]any{service: map[string]any{
			"image":   "ghcr.io/example/app:1",
			"volumes": []any{map[string]any{"type": "bind", "source": src, "target": "/host", "read_only": ro}},
		}}}
	}
	cases := []struct {
		name, repo, service, src string
		ro                       bool
		want                     string // "" = ok
	}{
		{"named service, right repo, read-only: ok", "observability", "node-exporter", "/", true, ""},
		{"named service but writable: denied", "observability", "node-exporter", "/", false, "may only be mounted read-only"},
		{"different service, same repo: denied", "observability", "loki", "/", true, "not allowed"},
		{"named service, wrong repo: denied", "some-other-repo", "node-exporter", "/", true, "not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := check(p, doc(tc.service, tc.src, tc.ro), tc.repo, "prod", nil)
			got := strings.Join(r.Violations, "\n")
			if tc.want == "" && got != "" {
				t.Fatalf("unexpected violations:\n%s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got:\n%s", tc.want, got)
			}
		})
	}
}

func TestBindMountsRootLoadValidation(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		f := filepath.Join(t.TempDir(), "policy.yml")
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}

	t.Run("root without services rejected", func(t *testing.T) {
		f := write(t, "bind_mounts:\n  - path: /\n    read_only: true\n    repos: [observability]\n")
		if _, err := Load(f); err == nil || !strings.Contains(err.Error(), "also requires services") {
			t.Fatalf("want a services-required error, got %v", err)
		}
	})

	t.Run("root with services accepted", func(t *testing.T) {
		f := write(t, "bind_mounts:\n  - path: /\n    read_only: true\n    repos: [observability]\n    services: [node-exporter]\n")
		p, err := Load(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.BindMounts) != 1 || len(p.BindMounts[0].Services) != 1 {
			t.Fatalf("got %+v", p.BindMounts)
		}
	})

	t.Run("non-root rule needs no services", func(t *testing.T) {
		f := write(t, "bind_mounts:\n  - path: /srv/swarm/{repo}/{env}\n    create: true\n")
		if _, err := Load(f); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBindHistory(t *testing.T) {
	binds := []Bind{{Service: "app", Source: "/srv/swarm/app/prod/uploads", Root: "/srv/swarm/app/prod"}}
	if v := CheckBindHistory(binds, map[string][]string{"/srv/swarm/app/prod": {"/srv/swarm/app/prod/uploads"}}); len(v) != 0 {
		t.Fatalf("same folder again must be ok: %v", v)
	}
	if v := CheckBindHistory(binds, map[string][]string{"/srv/swarm/app/prod": {"/srv/swarm/app/prod"}}); len(v) != 1 {
		t.Fatalf("folder below a formerly writable mount must be refused: %v", v)
	}
	w := WritableSources(append(binds, Bind{Source: "/srv/shared/certs", ReadOnly: true, Root: "/srv/shared/certs"}))
	if len(w) != 1 || w["/srv/swarm/app/prod"][0] != "/srv/swarm/app/prod/uploads" {
		t.Fatal(w)
	}
}

func TestPrefix(t *testing.T) {
	for rule, want := range map[string]string{
		"/srv/swarm/{repo}/{env}":   "/srv/swarm",
		"/srv/swarm/data-{stack}":   "/srv/swarm",
		"/mnt/nas/einsatz":          "/mnt/nas",
		"/{repo}":                   "/",
		"/srv/swarm/{repo}/x/{env}": "/srv/swarm",
	} {
		if got := (BindRule{Path: rule}).Prefix(); got != want {
			t.Errorf("Prefix(%s) = %s, want %s", rule, got, want)
		}
	}
}
