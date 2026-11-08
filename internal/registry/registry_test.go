package registry

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]Ref{
		"nginx":                             {Host: "docker.io", Repo: "library/nginx", Tag: "latest"},
		"nginx:1.27":                        {Host: "docker.io", Repo: "library/nginx", Tag: "1.27"},
		"traefik/whoami:v1":                 {Host: "docker.io", Repo: "traefik/whoami", Tag: "v1"},
		"ghcr.io/org/app:main":              {Host: "ghcr.io", Repo: "org/app", Tag: "main"},
		"localhost:5000/app":                {Host: "localhost:5000", Repo: "app", Tag: "latest"},
		"ghcr.io/org/app:main@sha256:abc":   {Host: "ghcr.io", Repo: "org/app", Tag: "main", Digest: "sha256:abc"},
		"registry.local:443/a/b/c@sha256:1": {Host: "registry.local:443", Repo: "a/b/c", Digest: "sha256:1"},
	} {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// Fake registry with token auth: HEAD -> 401 -> token (basic auth) -> HEAD with bearer.
func TestDigestWithTokenAuth(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			u, p, ok := r.BasicAuth()
			if !ok || u != "bot" || p != "s3cret" || r.URL.Query().Get("scope") != "repository:org/app:pull" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"token":"tok123"}`)
		case r.URL.Path == "/v2/org/app/manifests/main":
			if r.Header.Get("Authorization") != "Bearer tok123" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:org/app:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "image.index") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:feed")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")

	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("bot:s3cret"))
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{"`+host+`":{"auth":"`+auth+`"}}}`), 0o600)

	r := NewResolver(filepath.Join(dir, "config.json"))
	r.http = srv.Client()
	d, err := r.Digest(Parse(host + "/org/app:main"))
	if err != nil {
		t.Fatal(err)
	}
	if d != "sha256:feed" {
		t.Fatalf("digest = %s", d)
	}
	// second call uses the cached token
	if d, err = r.Digest(Parse(host + "/org/app:main")); err != nil || d != "sha256:feed" {
		t.Fatalf("cached: %s %v", d, err)
	}
	if _, err := r.Digest(Parse(host + "/org/missing:main")); err == nil {
		t.Fatal("expected error for missing image")
	}
}

// Fake registry: index -> linux/amd64 manifest -> config blob, behind the
// same token auth. Labels returns nothing until the background lookup is done.
func TestLabels(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"token":"tok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/org/app/manifests/sha256:index":
			fmt.Fprint(w, `{"manifests":[
				{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},
				{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}`)
		case "/v2/org/app/manifests/sha256:amd":
			fmt.Fprint(w, `{"config":{"digest":"sha256:cfg"}}`)
		case "/v2/org/app/blobs/sha256:cfg":
			fmt.Fprint(w, `{"config":{"Labels":{"org.opencontainers.image.revision":"8db45e6"}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")

	r := NewResolver("")
	r.http = srv.Client()
	wait := func(image string) (map[string]string, bool) {
		ref := Parse(image)
		if _, ok := r.Labels(ref); ok {
			t.Fatalf("%s: first call must not block", image)
		}
		for i := 0; i < 200; i++ {
			r.mu.Lock()
			l := r.labels[ref.Digest]
			r.mu.Unlock()
			if l != nil {
				return r.Labels(ref)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: lookup never finished", image)
		return nil, false
	}

	l, ok := wait(host + "/org/app:develop@sha256:index")
	if !ok || l["org.opencontainers.image.revision"] != "8db45e6" {
		t.Fatalf("labels = %v %v", l, ok)
	}
	// A failed lookup is cached as "no labels", not retried.
	if l, ok := wait(host + "/org/app@sha256:missing"); !ok || len(l) != 0 {
		t.Fatalf("missing: %v %v", l, ok)
	}
	if _, ok := r.Labels(Parse(host + "/org/app:develop")); ok {
		t.Fatal("no digest: want ok=false")
	}
}
