package render

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andipunz/swarm-gitops/internal/policy"
	"gopkg.in/yaml.v3"
)

const baseStack = `services:
  web:
    image: ghcr.io/example/web:${TAG:-main}
    env_file: [app.env]
    configs:
      - source: nginx
        target: /etc/nginx/nginx.conf
    secrets: [db]
    networks: [traefik-public, default]
    deploy:
      labels:
        traefik.enable: "true"
        traefik.http.routers.${STACK}.rule: Host("${STACK}.example.org")
configs:
  nginx:
    file: ./config/nginx.conf
secrets:
  db:
    driver: acme/secrets
networks:
  traefik-public:
    external: true
`

func setup(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func pol() *policy.Policy {
	p := policy.Default()
	p.SecretDriver = "acme/secrets"
	p.ImagePrefixes = []string{"ghcr.io/example/"}
	return p
}

func input(dir, commit string, files ...string) Input {
	return Input{
		Stack: "web-prod", Repo: "web", Env: "prod", GitHubEnv: "prod", Ref: "main", Commit: commit,
		WorkDir: dir, Files: files, Optional: map[string]bool{"stack.prod.yml": true}, VarsFile: "prod.env", Policy: pol(),
	}
}

func TestRenderOK(t *testing.T) {
	dir := setup(t, map[string]string{
		"stack.yml":         baseStack,
		"stack.prod.yml":    "services:\n  web:\n    deploy:\n      replicas: 3\n",
		"prod.env":          "TAG=v1.2.3\n",
		"app.env":           "FOO=bar\n",
		"config/nginx.conf": "worker_processes 1;\n",
	})
	res, err := Render(context.Background(), input(dir, "c1", "stack.yml", "stack.prod.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Violations) > 0 {
		t.Fatalf("violations: %v", res.Violations)
	}
	data, _ := os.ReadFile(res.Path)
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)
	if web["image"] != "ghcr.io/example/web:v1.2.3" {
		t.Errorf("image = %v (vars file not applied)", web["image"])
	}
	if _, ok := web["env_file"]; ok {
		t.Error("env_file must be removed after inlining")
	}
	if web["environment"].(map[string]any)["FOO"] != "bar" {
		t.Error("env_file not inlined")
	}
	labels := web["deploy"].(map[string]any)["labels"].(map[string]any)
	for k, want := range map[string]string{LabelManaged: "true", LabelRepo: "web", LabelEnv: "prod", LabelCommit: "c1", "traefik.enable": "true"} {
		if labels[k] != want {
			t.Errorf("label %s = %v, want %s", k, labels[k], want)
		}
	}
	if labels["traefik.http.routers.web-prod.rule"] != `Host("web-prod.example.org")` {
		t.Errorf("${STACK} not replaced in label key/value: %v", labels)
	}
	if r := web["deploy"].(map[string]any)["replicas"]; r != 3 {
		t.Errorf("replicas = %v (override not merged)", r)
	}
	cfg := doc["configs"].(map[string]any)["nginx"].(map[string]any)
	if name, _ := cfg["name"].(string); !strings.HasPrefix(name, "web-prod_") {
		t.Errorf("config not content-addressed: %v", cfg["name"])
	}
	sec := doc["secrets"].(map[string]any)["db"].(map[string]any)
	if sec["labels"].(map[string]any)[LabelRepo] != "web" {
		t.Error("secret labels missing (needed by the secrets plugin)")
	}

	// Same content on another commit: same spec hash (no redeploy).
	res2, err := Render(context.Background(), input(dir, "c2", "stack.yml", "stack.prod.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if res2.SpecHash != res.SpecHash {
		t.Error("spec hash must not depend on the commit")
	}
	// Changed config content: new hash.
	_ = os.WriteFile(filepath.Join(dir, "config/nginx.conf"), []byte("worker_processes 2;\n"), 0o600)
	res3, _ := Render(context.Background(), input(dir, "c3", "stack.yml", "stack.prod.yml"))
	if res3.SpecHash == res.SpecHash {
		t.Error("config change must change the spec hash")
	}
}

func TestRenderViolations(t *testing.T) {
	cases := map[string]struct {
		stack string
		want  string
	}{
		"docker socket": {`services:
  web:
    image: ghcr.io/example/web:1
    volumes: ["/var/run/docker.sock:/var/run/docker.sock"]
`, "bind mount"},
		"long bind": {`services:
  web:
    image: ghcr.io/example/web:1
    volumes: [{type: bind, source: /, target: /host}]
`, "bind mount"},
		"foreign image": {"services:\n  web:\n    image: evil/miner:latest\n", "not from an allowed registry"},
		"host network":  {"services:\n  web:\n    image: ghcr.io/example/web:1\n    networks: [hostnet]\nnetworks:\n  hostnet:\n    external: true\n    name: host\n", "external network"},
		"ports":         {"services:\n  web:\n    image: ghcr.io/example/web:1\n    ports: [\"443:443\"]\n", "published ports"},
		"file secret":   {"services:\n  web:\n    image: ghcr.io/example/web:1\n    secrets: [s]\nsecrets:\n  s:\n    file: ./s.txt\n", "not allowed for secrets"},
		"reserved label": {`services:
  web:
    image: ghcr.io/example/web:1
    deploy:
      labels:
        swarm-gitops.repo: other
`, "reserved"},
		"cap_add":       {"services:\n  web:\n    image: ghcr.io/example/web:1\n    cap_add: [SYS_ADMIN]\n", "capability"},
		"bind via opts": {"services:\n  web:\n    image: ghcr.io/example/web:1\n    volumes: [\"d:/d\"]\nvolumes:\n  d:\n    driver_opts: {type: none, o: bind, device: /etc}\n", "network filesystem"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := setup(t, map[string]string{"stack.yml": tc.stack, "s.txt": "x", "prod.env": ""})
			res, err := Render(context.Background(), input(dir, "c1", "stack.yml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(res.Violations, "\n"), tc.want) {
				t.Fatalf("want violation %q, got %v", tc.want, res.Violations)
			}
		})
	}
}

// Files outside .swarm must never be read by docker stack config.
func TestPrecheckBlocksFileExfiltration(t *testing.T) {
	for name, stack := range map[string]string{
		"env_file absolute": "services:\n  web:\n    image: ghcr.io/example/web:1\n    env_file: [/run/secrets/github-app-key]\n",
		"env_file escape":   "services:\n  web:\n    image: ghcr.io/example/web:1\n    env_file: ../../state.json\n",
		"config escape":     "services:\n  web:\n    image: ghcr.io/example/web:1\nconfigs:\n  c:\n    file: /etc/shadow\n",
		"variable path":     "services:\n  web:\n    image: ghcr.io/example/web:1\n    env_file: ${HOME}/x\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := setup(t, map[string]string{"stack.yml": stack, "prod.env": ""})
			res, err := Render(context.Background(), input(dir, "c1", "stack.yml"))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Violations) == 0 || res.Path != "" {
				t.Fatalf("expected precheck violation, got %+v", res)
			}
		})
	}
}

func TestRenderRejectsDockerVars(t *testing.T) {
	dir := setup(t, map[string]string{"stack.yml": "services:\n  web:\n    image: ghcr.io/example/web:1\n", "prod.env": "DOCKER_HOST=tcp://evil:2375\n"})
	if _, err := Render(context.Background(), input(dir, "c1", "stack.yml")); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected error, got %v", err)
	}
}
