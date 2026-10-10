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

func TestTokenAuth(t *testing.T) {
	var sawAppEndpoint bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/installation") || strings.Contains(r.URL.Path, "access_tokens") {
			sawAppEndpoint = true
		}
		if r.Header.Get("Authorization") != "Bearer mytoken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/repos/bw/app/git/blobs/b1" {
			io.WriteString(w, `{"content":"aGVsbG8=","encoding":"base64"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewWithToken("bw", srv.URL, "mytoken")
	data, err := c.Blob("app", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q", data)
	}
	if sawAppEndpoint {
		t.Fatal("token auth must never call the GitHub App installation endpoints")
	}
}

func TestCommitCheckRunsPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/bw/app/commits/c1/check-runs" || r.URL.Query().Get("filter") != "latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var runs []map[string]any
		switch r.URL.Query().Get("page") {
		case "1":
			for i := 0; i < 100; i++ {
				runs = append(runs, map[string]any{"name": "test", "status": "completed", "conclusion": "success", "app": map[string]any{"id": 15368}})
			}
		case "2":
			runs = append(runs, map[string]any{"name": "build", "status": "in_progress", "conclusion": nil, "app": map[string]any{"id": 15368}})
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "check_runs": runs})
	}))
	defer srv.Close()

	runs, err := NewWithToken("bw", srv.URL, "tok").CommitCheckRuns("app", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 101 {
		t.Fatalf("got %d runs, want 101 across both pages", len(runs))
	}
	if last := runs[100]; last.Name != "build" || last.Status != "in_progress" || last.Conclusion != "" || last.AppID != 15368 {
		t.Fatalf("last run = %+v", last)
	}
}

func TestBranchProtected(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	var protection, rules string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/bw/installation":
			io.WriteString(w, `{"id":42}`)
		case r.URL.Path == "/app/installations/42/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "inst-tok", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/repos/bw/app/branches/main/protection":
			if protection == "404" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, protection)
		case r.URL.Path == "/repos/bw/app/rules/branches/main":
			io.WriteString(w, rules)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := New("bw", srv.URL, 7, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	// Classic protection or a ruleset must actually require a pull request
	// WITH at least one approval: weaker rules (blocking force-pushes,
	// requiring only a status check, or a PR with zero required approvals)
	// must not count, because a collaborator could still push directly, or
	// open and self-merge an unreviewed pull request.
	cases := []struct {
		name, protection, rules string
		want                    bool
	}{
		{"classic with required reviews", `{"required_pull_request_reviews":{"required_approving_review_count":1}}`, `[]`, true},
		{"classic protected but no review requirement", `{}`, `[]`, false},
		{"classic pull request required but zero approvals", `{"required_pull_request_reviews":{"required_approving_review_count":0}}`, `[]`, false},
		{"unprotected, ruleset requires pull request with approval", "404", `[{"type":"pull_request","parameters":{"required_approving_review_count":1}}]`, true},
		{"unprotected, ruleset requires pull request but zero approvals", "404", `[{"type":"pull_request","parameters":{"required_approving_review_count":0}}]`, false},
		{"unprotected, ruleset requires pull request, no parameters", "404", `[{"type":"pull_request"}]`, false},
		{"unprotected, ruleset only blocks force-push", "404", `[{"type":"non_fast_forward"}]`, false},
		{"unprotected, ruleset only requires status checks", "404", `[{"type":"required_status_checks"}]`, false},
		{"unprotected, no rules", "404", `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			protection, rules = tc.protection, tc.rules
			got, err := c.BranchProtected("app", "main")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("BranchProtected = %v, want %v", got, tc.want)
			}
		})
	}
}

// A branch without .swarm must simply come back with an empty SwarmTree, not
// fail the listing for every branch of the repo. The fake rejects
// file(path:".swarm") the way GitHub does (a NOT_FOUND error), so a
// regression back to that query fails here.
func TestBranchesWithoutSwarm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "file(path:") {
			io.WriteString(w, `{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve file for path '.swarm'."}]}`)
			return
		}
		io.WriteString(w, `{"data":{"repository":{"refs":{"pageInfo":{"hasNextPage":false},"nodes":[
		  {"name":"develop","target":{"oid":"c1","messageHeadline":"m","tree":{"oid":"r1","entries":[{"name":"README.md","type":"blob","oid":"x"},{"name":".swarm","type":"tree","oid":"s1"}]},"parents":{"totalCount":1,"nodes":[{"tree":{"oid":"r0"}}]}}},
		  {"name":"feature/x","target":{"oid":"c2","messageHeadline":"m","tree":{"oid":"r2","entries":[{"name":"README.md","type":"blob","oid":"x"}]},"parents":{"totalCount":1,"nodes":[{"tree":{"oid":"r0"}}]}}},
		  {"name":"odd","target":{"oid":"c3","messageHeadline":"m","tree":{"oid":"r3","entries":[{"name":".swarm","type":"blob","oid":"f1"}]},"parents":{"totalCount":0,"nodes":[]}}}]}}}}`)
	}))
	defer srv.Close()

	c := NewWithToken("bw", srv.URL, "tok")
	bs, err := c.Branches("app")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range bs {
		got[b.Name] = b.SwarmTree
	}
	want := map[string]string{"develop": "s1", "feature/x": "", "odd": ""}
	for name, tree := range want {
		if g, ok := got[name]; !ok || g != tree {
			t.Errorf("%s: SwarmTree = %q (listed: %v), want %q", name, g, ok, tree)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d branches, want %d", len(got), len(want))
	}
}
