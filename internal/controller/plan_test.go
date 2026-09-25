package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/bergwacht-bayern/swarm-gitops/internal/gh"
)

const deployYML = `
environments:
  prod:
    ref: main
  staging:
    ref: develop
  sandbox:
    ref: "sandbox/*"
`

func ptr(s string) *string { return &s }

func baseInput() PlanInput {
	return PlanInput{
		Repos: []gh.Repo{{Name: "app", DefaultBranch: "main", DefaultHead: "c1", DeployFile: ptr(deployYML)}},
		Branches: map[string][]gh.Branch{"app": {
			{Name: "main", Commit: "c1", SwarmTree: "t1"},
			{Name: "develop", Commit: "c2", SwarmTree: "t1"},
			{Name: "sandbox/feat-x", Commit: "c3", SwarmTree: "t1"},
			{Name: "feature/other", Commit: "c4", SwarmTree: "t1"},
		}},
		BranchErr:  map[string]error{},
		SwarmFiles: func(string, string) (map[string]bool, error) { return map[string]bool{"stack.yml": true}, nil },
		Current:    map[string]StackInfo{},
	}
}

func stacks(ts []Target) []string {
	var s []string
	for _, t := range ts {
		s = append(s, t.Stack)
	}
	return s
}

func TestPlanTargets(t *testing.T) {
	p := MakePlan(baseInput())
	got := strings.Join(stacks(p.Targets), ",")
	want := "app-prod,app-sandbox-feat-x,app-staging"
	if got != want {
		t.Fatalf("targets = %s, want %s", got, want)
	}
	for _, tg := range p.Targets {
		if tg.Stack == "app-sandbox-feat-x" && tg.GitHubEnv != "sandbox/feat-x" {
			t.Errorf("github env = %s", tg.GitHubEnv)
		}
	}
}

func TestPlanRemovals(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(in *PlanInput)
		stack      string
		wantRemove string // substring of reason, "" = must be kept
		wantOrphan bool
	}{
		{"archived repo", func(in *PlanInput) { in.Repos[0].Archived = true }, "app-prod", "archived", false},
		{"deploy.yml removed", func(in *PlanInput) { in.Repos[0].DeployFile = nil }, "app-prod", "deploy.yml", false},
		{"env removed from deploy.yml", func(in *PlanInput) {
			in.Repos[0].DeployFile = ptr("environments:\n  prod:\n    ref: main\n")
		}, "app-staging", "no longer exists", false},
		{"sandbox branch deleted", func(in *PlanInput) {
			in.Branches["app"] = in.Branches["app"][:2]
		}, "app-sandbox-feat-x", "no longer exists", false},
		{".swarm removed on branch", func(in *PlanInput) { in.Branches["app"][1].SwarmTree = "" }, "app-staging", ".swarm/", false},
		{"compose files removed", func(in *PlanInput) {
			in.SwarmFiles = func(string, string) (map[string]bool, error) { return map[string]bool{"deploy.yml": true}, nil }
		}, "app-prod", "compose files", false},
		{"env no longer enabled", func(in *PlanInput) {
			in.Allowed = map[string][]string{"app": {"staging"}}
		}, "app-prod", "not enabled", false},
		// uncertainty: never remove
		{"branch listing failed", func(in *PlanInput) { in.BranchErr["app"] = errors.New("timeout") }, "app-prod", "", false},
		{"tree read failed", func(in *PlanInput) {
			in.SwarmFiles = func(string, string) (map[string]bool, error) { return nil, errors.New("502") }
		}, "app-prod", "", false},
		{"broken deploy.yml", func(in *PlanInput) { in.Repos[0].DeployFile = ptr("environments: [") }, "app-prod", "", false},
		{"repo not visible", func(in *PlanInput) { in.Repos = nil }, "app-prod", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Current = map[string]StackInfo{
				"app-prod": {Repo: "app", Env: "prod"}, "app-staging": {Repo: "app", Env: "staging"},
				"app-sandbox-feat-x": {Repo: "app", Env: "sandbox"},
			}
			tc.mutate(&in)
			p := MakePlan(in)
			reason, removed := p.Remove[tc.stack]
			if tc.wantRemove == "" && removed {
				t.Fatalf("%s must be kept, got removal: %s", tc.stack, reason)
			}
			if tc.wantRemove != "" && !strings.Contains(reason, tc.wantRemove) {
				t.Fatalf("%s: want removal containing %q, got %q (removed=%v)", tc.stack, tc.wantRemove, reason, removed)
			}
			if _, orphan := p.Orphaned[tc.stack]; orphan != tc.wantOrphan {
				t.Fatalf("orphaned = %v, want %v", orphan, tc.wantOrphan)
			}
		})
	}
}

func TestPlanSkippedWhenNotEnabled(t *testing.T) {
	in := baseInput()
	in.Allowed = map[string][]string{"app": {"staging", "sandbox"}}
	p := MakePlan(in)
	if len(p.Skipped) != 1 || p.Skipped[0].Stack != "app-prod" {
		t.Fatalf("skipped = %v", stacks(p.Skipped))
	}
	if strings.Contains(strings.Join(stacks(p.Targets), ","), "app-prod") {
		t.Fatal("prod must not be a target")
	}
}

func TestPlanStackNameCollision(t *testing.T) {
	in := baseInput()
	in.Repos = append(in.Repos, gh.Repo{Name: "App", DeployFile: ptr("environments:\n  prod:\n    ref: main\n")})
	in.Branches["App"] = []gh.Branch{{Name: "main", Commit: "x", SwarmTree: "t"}}
	p := MakePlan(in)
	if p.ConfigErrors["app"] == nil && p.ConfigErrors["App"] == nil {
		t.Fatal("expected a collision error")
	}
	n := 0
	for _, tg := range p.Targets {
		if tg.Stack == "app-prod" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("app-prod targets = %d", n)
	}
}

func TestEmptyCommit(t *testing.T) {
	if !(gh.Branch{TreeOid: "a", ParentTree: "a"}).EmptyCommit() {
		t.Fatal("same tree as parent must be an empty commit")
	}
	if (gh.Branch{TreeOid: "a", ParentTree: ""}).EmptyCommit() {
		t.Fatal("merge/root commit must not count as empty")
	}
}
