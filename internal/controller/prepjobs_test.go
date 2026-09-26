package controller

import (
	"testing"

	"github.com/andipunz/swarm-gitops/internal/policy"
)

func TestPrepJobsGroupsByConstraints(t *testing.T) {
	binds := []policy.Bind{
		{Prefix: "/srv/swarm", Source: "/srv/swarm/app/prod", Create: true, Constraints: nil},
		{Prefix: "/mnt/nas", Source: "/mnt/nas/x", Create: false, Constraints: []string{"node.hostname == nas1"}},
	}
	jobs := prepJobs(binds)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
}

func TestPrepJobsPrefixReadOnly(t *testing.T) {
	cases := []struct {
		name string
		bs   []policy.Bind
		want map[string]bool // prefix -> want ReadOnly
	}{
		{
			name: "check-only prefix is read-only",
			bs: []policy.Bind{
				{Prefix: "/", Source: "/", Create: false},
			},
			want: map[string]bool{"/": true},
		},
		{
			name: "any create spec makes the whole prefix writable",
			bs: []policy.Bind{
				{Prefix: "/srv/swarm", Source: "/srv/swarm/app/prod", Create: true},
				{Prefix: "/srv/swarm", Source: "/srv/swarm/app/prod/logs", Create: false},
			},
			want: map[string]bool{"/srv/swarm": false},
		},
		{
			name: "independent prefixes in the same job keep independent modes",
			bs: []policy.Bind{
				{Prefix: "/", Source: "/", Create: false},
				{Prefix: "/srv/swarm", Source: "/srv/swarm/app/prod", Create: true},
			},
			want: map[string]bool{"/": true, "/srv/swarm": false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			jobs := prepJobs(c.bs)
			if len(jobs) != 1 {
				t.Fatalf("got %d jobs, want 1", len(jobs))
			}
			got := map[string]bool{}
			for _, p := range jobs[0].Prefixes {
				got[p.Path] = p.ReadOnly
			}
			for prefix, want := range c.want {
				if got[prefix] != want {
					t.Errorf("prefix %s: got ReadOnly=%v, want %v", prefix, got[prefix], want)
				}
			}
		})
	}
}
