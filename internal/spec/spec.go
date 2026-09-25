// Package spec parses .swarm/deploy.yml and turns environments + branches
// into concrete deployment targets (stacks).
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Env is one environment of a repo.
type Env struct {
	Ref   string   `yaml:"ref"`   // branch name or glob ("sandbox/*")
	Files []string `yaml:"files"` // compose files relative to .swarm/, merged in order
	Vars  string   `yaml:"vars"`  // optional dotenv file relative to .swarm/
	URL   string   `yaml:"url"`   // optional environment URL shown in GitHub
	// Owner ("uid:gid") for bind mount folders the controller creates.
	BindOwner string `yaml:"bind_owner"`
}

// File is .swarm/deploy.yml.
type File struct {
	Environments map[string]Env `yaml:"environments"`
}

var envNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

var ownerRe = regexp.MustCompile(`^[0-9]{1,10}:[0-9]{1,10}$`)

// Parse parses and validates deploy.yml.
func Parse(data []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("deploy.yml: %w", err)
	}
	if len(f.Environments) == 0 {
		return nil, fmt.Errorf("deploy.yml: no environments defined")
	}
	for name, e := range f.Environments {
		if !envNameRe.MatchString(name) {
			return nil, fmt.Errorf("deploy.yml: invalid environment name %q (lowercase letters, digits, dashes, max 20)", name)
		}
		if e.Ref == "" {
			return nil, fmt.Errorf("deploy.yml: environment %q has no ref", name)
		}
		if e.BindOwner != "" && !ownerRe.MatchString(e.BindOwner) {
			return nil, fmt.Errorf("deploy.yml: environment %q: bind_owner must be \"uid:gid\" (numbers)", name)
		}
		if _, err := path.Match(e.Ref, ""); err != nil {
			return nil, fmt.Errorf("deploy.yml: environment %q: bad ref pattern: %v", name, err)
		}
		for _, p := range append(append([]string{}, e.Files...), e.Vars) {
			if p == "" {
				continue
			}
			if !SafeRelPath(p) {
				return nil, fmt.Errorf("deploy.yml: environment %q: path %q must be relative and stay inside .swarm/", name, p)
			}
		}
	}
	return &f, nil
}

// EnvNames returns environment names sorted.
func (f *File) EnvNames() []string {
	var names []string
	for n := range f.Environments {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ComposeFiles returns the configured files, or the default
// [stack.yml, stack.<env>.yml] where the second one is optional.
func (e Env) ComposeFiles(env string) (files []string, optional map[string]bool) {
	if len(e.Files) > 0 {
		return e.Files, map[string]bool{}
	}
	o := "stack." + env + ".yml"
	return []string{"stack.yml", o}, map[string]bool{o: true}
}

// IsGlob reports whether the ref is a pattern (one stack per matching branch).
func (e Env) IsGlob() bool { return strings.ContainsAny(e.Ref, "*?[") }

// Matches reports whether a branch belongs to the environment.
func (e Env) Matches(branch string) bool {
	if !e.IsGlob() {
		return e.Ref == branch
	}
	ok, _ := path.Match(e.Ref, branch)
	return ok
}

// SafeRelPath reports whether p is relative and cannot escape its base dir.
func SafeRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "$") {
		return false
	}
	c := path.Clean(p)
	return c != ".." && !strings.HasPrefix(c, "../")
}

var nonSlug = regexp.MustCompile(`[^a-z0-9-]+`)

// Slug lowercases and replaces everything outside [a-z0-9-] with dashes.
func Slug(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

// MaxStackName leaves room for "_<service>" within Swarm's 63 char limit.
const MaxStackName = 40

// StackName builds <repo>-<env>[-<branch>], shortened with a hash if needed.
func StackName(repo, env string, e Env, branch string) string {
	name := Slug(repo) + "-" + env
	if e.IsGlob() {
		name += "-" + Slug(branchSuffix(e.Ref, branch))
	}
	if len(name) <= MaxStackName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:MaxStackName-7], "-") + "-" + hex.EncodeToString(sum[:])[:6]
}

// GitHubEnvironment is the environment name shown in GitHub.
func GitHubEnvironment(env string, e Env, branch string) string {
	if e.IsGlob() {
		return env + "/" + branchSuffix(e.Ref, branch)
	}
	return env
}

// branchSuffix strips the literal directory prefix of a glob ("sandbox/*" + "sandbox/x" -> "x").
func branchSuffix(pattern, branch string) string {
	i := strings.IndexAny(pattern, "*?[")
	prefix := pattern[:i]
	if j := strings.LastIndex(prefix, "/"); j >= 0 && strings.HasPrefix(branch, prefix[:j+1]) {
		return branch[j+1:]
	}
	return branch
}

// ParseDotenv parses KEY=VALUE lines (comments, blank lines, export prefix,
// single/double quotes). Used for the per-environment vars file.
func ParseDotenv(data []byte) (map[string]string, error) {
	res := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", i+1)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		} else if j := strings.Index(v, " #"); j >= 0 {
			v = strings.TrimSpace(v[:j])
		}
		res[k] = v
	}
	return res, nil
}
