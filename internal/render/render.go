// Package render turns the files of one environment into the final stack file:
// precheck -> `docker stack config` (merge + interpolation, same code as
// `docker stack deploy`) -> policy -> controller labels -> content-addressed
// config names -> stack.rendered.yml.
package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bergwacht-bayern/swarm-gitops/internal/policy"
	"github.com/bergwacht-bayern/swarm-gitops/internal/spec"
	"gopkg.in/yaml.v3"
)

// Controller labels, set on every service (and secret) of a managed stack.
const (
	LabelManaged = "swarm-gitops.managed"
	LabelRepo    = "swarm-gitops.repo"
	LabelEnv     = "swarm-gitops.env"
	LabelGHEnv   = "swarm-gitops.github-env"
	LabelRef     = "swarm-gitops.ref"
	LabelStack   = "swarm-gitops.stack"
	LabelCommit  = "swarm-gitops.commit"
	LabelSpec    = "swarm-gitops.spec"
	LabelImage   = "swarm-gitops.image"
	LabelURL     = "swarm-gitops.url"
)

// Input describes one environment of one repo at one commit.
type Input struct {
	Stack, Repo, Env, GitHubEnv, Ref, Commit, URL string
	WorkDir                                       string // .swarm files materialised here
	Files                                         []string
	Optional                                      map[string]bool
	VarsFile                                      string
	Policy                                        *policy.Policy
}

// Result of a render. Violations != nil means the stack must not be deployed.
type Result struct {
	Path       string
	SpecHash   string
	Services   []string
	Images     map[string]string
	Violations []string
}

// ErrInvalid wraps problems caused by the repo content (shown to devs).
var ErrInvalid = errors.New("invalid stack")

var varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Render builds and checks the stack.
func Render(ctx context.Context, in Input) (*Result, error) {
	var files []string
	var violations []string
	for _, f := range in.Files {
		p := filepath.Join(in.WorkDir, f)
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			if in.Optional[f] {
				continue
			}
			return nil, invalid("compose file .swarm/%s does not exist", f)
		}
		if err != nil {
			return nil, err
		}
		violations = append(violations, policy.PrecheckRaw(".swarm/"+f, data)...)
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, invalid("no compose files found")
	}
	if len(violations) > 0 {
		return &Result{Violations: violations}, nil
	}

	env := map[string]string{}
	if in.VarsFile != "" {
		data, err := os.ReadFile(filepath.Join(in.WorkDir, in.VarsFile))
		if err != nil {
			return nil, invalid("vars file .swarm/%s: %v", in.VarsFile, err)
		}
		vars, err := spec.ParseDotenv(data)
		if err != nil {
			return nil, invalid("vars file .swarm/%s: %v", in.VarsFile, err)
		}
		for k, v := range vars {
			up := strings.ToUpper(k)
			if !varNameRe.MatchString(k) || strings.HasPrefix(up, "DOCKER_") || strings.HasPrefix(up, "COMPOSE_") ||
				up == "PATH" || up == "HOME" || strings.HasPrefix(up, "SWARM_GITOPS") {
				return nil, invalid("vars file .swarm/%s: variable %q is not allowed", in.VarsFile, k)
			}
			env[k] = v
		}
	}
	for k, v := range map[string]string{"ENV": in.Env, "GIT_SHA": in.Commit, "GIT_REF": in.Ref, "STACK": in.Stack, "REPO": in.Repo} {
		env[k] = v
	}

	rendered, err := stackConfig(ctx, in.WorkDir, files, env)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return nil, fmt.Errorf("parse rendered stack: %w", err)
	}
	if v := in.Policy.Check(doc, policy.Context{Stack: in.Stack, Env: in.Env, WorkDir: in.WorkDir}); len(v) > 0 {
		return &Result{Violations: v}, nil
	}

	res := &Result{Images: map[string]string{}}
	// Swarm configs are immutable: name them by content so a change rolls the service.
	hashDoc := map[string]any{}
	configs, _ := doc["configs"].(map[string]any)
	for _, name := range sortedKeys(configs) {
		cfg, _ := configs[name].(map[string]any)
		f, ok := cfg["file"].(string)
		if !ok {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			rel, _ := filepath.Rel(in.WorkDir, f)
			return nil, invalid("config %s: file .swarm/%s not found", name, rel)
		}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])[:10]
		cfg["name"] = cut(in.Stack+"_"+h+"_"+name, 64)
		hashDoc["config:"+name] = h
	}

	keyVars := strings.NewReplacer("${STACK}", in.Stack, "${ENV}", in.Env)
	labels := map[string]string{
		LabelManaged: "true", LabelRepo: in.Repo, LabelEnv: in.Env, LabelGHEnv: in.GitHubEnv,
		LabelRef: in.Ref, LabelStack: in.Stack,
	}
	if in.URL != "" {
		labels[LabelURL] = in.URL
	}
	services, _ := doc["services"].(map[string]any)
	for _, name := range sortedKeys(services) {
		svc, _ := services[name].(map[string]any)
		delete(svc, "env_file") // already inlined into environment by docker stack config
		deploy, _ := svc["deploy"].(map[string]any)
		if deploy == nil {
			deploy = map[string]any{}
			svc["deploy"] = deploy
		}
		// Compose never interpolates label keys; Traefik router names must be
		// unique per stack, so ${STACK} / ${ENV} are replaced in keys here.
		l := map[string]string{}
		for k, v := range labelMap(deploy["labels"]) {
			l[keyVars.Replace(k)] = v
		}
		if cl := labelMap(svc["labels"]); len(cl) > 0 {
			m := map[string]any{}
			for k, v := range cl {
				m[keyVars.Replace(k)] = v
			}
			svc["labels"] = m
		}
		for k, v := range labels {
			l[k] = v
		}
		img, _ := svc["image"].(string)
		l[LabelImage] = img
		deploy["labels"] = l
		res.Services = append(res.Services, name)
		res.Images[name] = img
	}
	secrets, _ := doc["secrets"].(map[string]any)
	for _, name := range sortedKeys(secrets) {
		s, _ := secrets[name].(map[string]any)
		l := labelMap(s["labels"])
		for _, k := range []string{LabelManaged, LabelRepo, LabelEnv, LabelStack} {
			l[k] = labels[k]
		}
		s["labels"] = l
	}

	// Spec hash: everything except the commit label and absolute config file
	// paths (they contain the commit dir), so unchanged stacks aren't redeployed.
	filePaths := map[string]any{}
	for _, name := range sortedKeys(configs) {
		if cfg, _ := configs[name].(map[string]any); cfg != nil {
			if f, ok := cfg["file"]; ok {
				filePaths[name] = f
				delete(cfg, "file")
			}
		}
	}
	hashDoc["stack"] = doc
	hb, err := yaml.Marshal(hashDoc)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(hb)
	res.SpecHash = hex.EncodeToString(sum[:])[:16]
	for name, f := range filePaths {
		configs[name].(map[string]any)["file"] = f
	}

	for _, name := range sortedKeys(services) {
		svc, _ := services[name].(map[string]any)
		deploy, _ := svc["deploy"].(map[string]any)
		l, _ := deploy["labels"].(map[string]string)
		l[LabelCommit] = in.Commit
		l[LabelSpec] = res.SpecHash
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	res.Path = filepath.Join(in.WorkDir, "stack.rendered.yml")
	return res, os.WriteFile(res.Path, out, 0o600)
}

func stackConfig(ctx context.Context, dir string, files []string, env map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"stack", "config"}
	for _, f := range files {
		args = append(args, "-c", f)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		msg = strings.ReplaceAll(msg, dir+"/", "")
		if msg == "" {
			msg = err.Error()
		}
		return nil, invalid("docker stack config: %s", msg)
	}
	return stdout.Bytes(), nil
}

func labelMap(v any) map[string]string {
	res := map[string]string{}
	switch l := v.(type) {
	case map[string]any:
		for k, val := range l {
			res[k] = fmt.Sprint(val)
		}
	case []any:
		for _, e := range l {
			if s, ok := e.(string); ok {
				k, val, _ := strings.Cut(s, "=")
				res[k] = val
			}
		}
	}
	return res
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
