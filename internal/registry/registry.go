// Package registry resolves image tags to digests via the Registry v2 API,
// using credentials from a docker config.json (the same file used for
// --with-registry-auth). Stdlib only.
package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Ref is a parsed image reference.
type Ref struct {
	Host, Repo, Tag, Digest string
}

// Parse parses references like nginx, ghcr.io/org/app:main, app:1@sha256:...
func Parse(image string) Ref {
	var r Ref
	if i := strings.Index(image, "@"); i >= 0 {
		r.Digest = image[i+1:]
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i:], "/") {
		r.Tag = image[i+1:]
		image = image[:i]
	}
	first, rest, hasSlash := strings.Cut(image, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Host, r.Repo = first, rest
	} else {
		r.Host, r.Repo = "docker.io", image
		if !hasSlash {
			r.Repo = "library/" + image
		}
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r
}

// Name returns host/repo:tag as used in service specs.
func (r Ref) Name() string {
	s := r.Host + "/" + r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	return s
}

// Resolver resolves tags to digests.
type Resolver struct {
	ConfigFile string // docker config.json path; empty = anonymous only
	http       *http.Client
	mu         sync.Mutex
	tokens     map[string]string // realm+scope -> bearer token
}

// NewResolver creates a resolver.
func NewResolver(configFile string) *Resolver {
	return &Resolver{ConfigFile: configFile, http: &http.Client{Timeout: 20 * time.Second}, tokens: map[string]string{}}
}

var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Digest returns the current digest of ref's tag.
func (r *Resolver) Digest(ref Ref) (string, error) {
	if ref.Tag == "" {
		return ref.Digest, nil
	}
	host := ref.Host
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, ref.Repo, ref.Tag)
	scope := "repository:" + ref.Repo + ":pull"
	resp, err := r.head(url, r.cachedToken(host, scope))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		tok, err := r.token(ref.Host, resp.Header.Get("WWW-Authenticate"), scope)
		if err != nil {
			return "", err
		}
		resp, err = r.head(url, "Bearer "+tok)
		if err != nil {
			return "", err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HEAD %s: %s", url, resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", errors.New("registry did not return Docker-Content-Digest")
	}
	return d, nil
}

func (r *Resolver) head(url, auth string) (*http.Response, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestTypes)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

func (r *Resolver) cachedToken(host, scope string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.tokens[host+" "+scope]; t != "" {
		return "Bearer " + t
	}
	return ""
}

// token performs the bearer token dance described by WWW-Authenticate.
func (r *Resolver) token(host, challenge, scope string) (string, error) {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return "", fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	params := map[string]string{}
	for _, p := range strings.Split(strings.TrimPrefix(challenge, "Bearer "), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		params[k] = strings.Trim(v, `"`)
	}
	if params["scope"] != "" {
		scope = params["scope"]
	}
	req, err := http.NewRequest("GET", params["realm"]+"?service="+params["service"]+"&scope="+scope, nil)
	if err != nil {
		return "", err
	}
	if user, pass, ok := r.credentials(host); ok {
		req.SetBasicAuth(user, pass)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("registry token for %s: %s %s", host, resp.Status, b)
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	tok := out.Token
	if tok == "" {
		tok = out.AccessToken
	}
	regHost := host
	if host == "docker.io" {
		regHost = "registry-1.docker.io"
	}
	r.mu.Lock()
	r.tokens[regHost+" "+scope] = tok
	r.mu.Unlock()
	return tok, nil
}

// credentials reads auths from docker config.json (credential helpers are not supported).
func (r *Resolver) credentials(host string) (string, string, bool) {
	file := r.ConfigFile
	if file == "" {
		if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
			file = filepath.Join(dir, "config.json")
		}
	}
	if file == "" {
		return "", "", false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", "", false
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return "", "", false
	}
	keys := []string{host, "https://" + host}
	if host == "docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io")
	}
	for _, k := range keys {
		a, ok := cfg.Auths[k]
		if !ok {
			continue
		}
		if a.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(a.Auth)
			if err == nil {
				u, p, _ := strings.Cut(string(dec), ":")
				return u, p, true
			}
		}
		if a.Username != "" {
			return a.Username, a.Password, true
		}
	}
	return "", "", false
}
