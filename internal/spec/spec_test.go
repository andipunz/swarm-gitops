package spec

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	f, err := Parse([]byte("environments:\n  prod:\n    ref: main\n    url: https://x\n  sandbox:\n    ref: 'sandbox/*'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.EnvNames(), ","); got != "prod,sandbox" {
		t.Fatal(got)
	}
	bad := map[string]string{
		"unknown key":   "environments:\n  prod:\n    ref: main\n    replicas: 3\n",
		"no ref":        "environments:\n  prod:\n    files: [stack.yml]\n",
		"bad name":      "environments:\n  Prod:\n    ref: main\n",
		"escaping path": "environments:\n  prod:\n    ref: main\n    files: [../../etc/passwd]\n",
		"absolute vars": "environments:\n  prod:\n    ref: main\n    vars: /run/secrets/x\n",
		"empty":         "environments: {}\n",
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestStackName(t *testing.T) {
	glob := Env{Ref: "sandbox/*"}
	if got := StackName("Einsatz_App", "prod", Env{Ref: "main"}, "main"); got != "einsatz-app-prod" {
		t.Fatal(got)
	}
	if got := StackName("app", "sandbox", glob, "sandbox/Feat.X"); got != "app-sandbox-feat-x" {
		t.Fatal(got)
	}
	long := StackName(strings.Repeat("very-long-repository-name-", 3), "staging", Env{Ref: "develop"}, "develop")
	if len(long) > MaxStackName {
		t.Fatalf("%s is %d chars", long, len(long))
	}
	if GitHubEnvironment("sandbox", glob, "sandbox/feat-x") != "sandbox/feat-x" {
		t.Fatal("github env")
	}
}

func TestMatches(t *testing.T) {
	e := Env{Ref: "sandbox/*"}
	if !e.Matches("sandbox/a") || e.Matches("sandbox/a/b") || e.Matches("main") {
		t.Fatal("glob match")
	}
	if !(Env{Ref: "main"}).Matches("main") {
		t.Fatal("exact match")
	}
}

func TestSafeRelPath(t *testing.T) {
	for p, want := range map[string]bool{
		"stack.yml": true, "config/nginx.conf": true, "./a": true,
		"/etc/passwd": false, "../x": false, "a/../../x": false, "${HOME}/x": false, "": false, "..": false,
	} {
		if SafeRelPath(p) != want {
			t.Errorf("SafeRelPath(%q) != %v", p, want)
		}
	}
}

func TestParseDotenv(t *testing.T) {
	m, err := ParseDotenv([]byte("# c\nA=1\nexport B=\"two words\"\nC='x=y'\nD=v # comment\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["A"] != "1" || m["B"] != "two words" || m["C"] != "x=y" || m["D"] != "v" {
		t.Fatalf("%v", m)
	}
	if _, err := ParseDotenv([]byte("NOEQUALS\n")); err == nil {
		t.Fatal("expected error")
	}
}
