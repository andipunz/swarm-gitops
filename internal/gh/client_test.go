package gh

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppAuthGraphQLAndETag(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var tokenCalls, fullResponses atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/orgs/bw/installation" || r.URL.Path == "/app/installations/42/access_tokens":
			// verify the app JWT
			parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
			if !strings.Contains(string(claims), `"iss":"7"`) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/orgs/bw/installation" {
				io.WriteString(w, `{"id":42}`)
				return
			}
			tokenCalls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"token": "inst-tok", "expires_at": time.Now().Add(time.Hour)})
		case auth != "Bearer inst-tok":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/graphql":
			io.WriteString(w, `{"data":{"organization":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[
			  {"name":"app","isArchived":false,"isDisabled":false,"defaultBranchRef":{"name":"main","target":{"oid":"c1"}},"deploy":{"text":"environments: {}","isTruncated":false}},
			  {"name":"old","isArchived":true,"isDisabled":false,"defaultBranchRef":null,"deploy":null},
			  {"name":"docs","isArchived":false,"isDisabled":false,"defaultBranchRef":{"name":"main","target":{"oid":"c9"}},"deploy":null}]}}}}`)
		case r.URL.Path == "/repos/bw/app/git/trees/t1":
			if r.Header.Get("If-None-Match") == `"etag1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			fullResponses.Add(1)
			w.Header().Set("ETag", `"etag1"`)
			io.WriteString(w, `{"tree":[{"path":"stack.yml","mode":"100644","type":"blob","sha":"b1","size":10}],"truncated":false}`)
		case r.URL.Path == "/orgs/bw/properties/values":
			io.WriteString(w, `[{"repository_name":"app","properties":[{"property_name":"swarm-environments","value":["prod","staging"]}]},
			                    {"repository_name":"docs","properties":[{"property_name":"swarm-environments","value":"sandbox"}]}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := New("bw", srv.URL, 7, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := c.ListRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 || repos[0].DeployFile == nil || !repos[1].Archived || repos[2].DeployFile != nil || repos[0].DefaultHead != "c1" {
		t.Fatalf("repos = %+v", repos)
	}
	for i := 0; i < 2; i++ {
		tree, err := c.Tree("app", "t1")
		if err != nil || len(tree) != 1 || tree[0].Path != "stack.yml" {
			t.Fatalf("tree %d: %+v %v", i, tree, err)
		}
	}
	if fullResponses.Load() != 1 {
		t.Fatalf("second GET should be served from the ETag cache, got %d full responses", fullResponses.Load())
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("installation token should be cached, got %d calls", tokenCalls.Load())
	}
	props, err := c.EnvProperties("swarm-environments")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(props["app"], ",") != "prod,staging" || strings.Join(props["docs"], ",") != "sandbox" {
		t.Fatalf("props = %v", props)
	}
	if _, err := c.Blob("app", "missing"); !IsNotFound(err) {
		t.Fatalf("expected 404, got %v", err)
	}
}
