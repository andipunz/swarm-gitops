package controller

import (
	"context"
	"time"

	"github.com/andipunz/swarm-gitops/internal/render"
	"github.com/andipunz/swarm-gitops/internal/state"
)

// StackReplicas is one service's replica counts, for the /metrics endpoint.
type StackReplicas struct {
	Stack, Service string
	Desired        int // -1 = no fixed desired count (global service)
	Running        int
}

// StackSource is where a managed stack was deployed from, for the /metrics endpoint.
type StackSource struct {
	Stack, Repo, Env, Branch, Commit string
}

// MetricsSnapshot is the point-in-time data exposed at /metrics.
type MetricsSnapshot struct {
	Stacks         []StackReplicas
	Sources        []StackSource
	LastScan       time.Time
	LastScanError  bool
	LastImageCheck time.Time
	Orphaned       int
	PruneBlocked   int
}

// Metrics gathers the current data for the /metrics endpoint. Every value
// comes from calls the controller already makes elsewhere (ManagedStacks,
// service task state) or already tracks in its own state — no new Docker
// privilege beyond what serving the admin API and doing deploys already needs.
func (c *Controller) Metrics(ctx context.Context) (*MetricsSnapshot, error) {
	stacks, err := c.docker.ManagedStacks(ctx)
	if err != nil {
		return nil, err
	}
	m := &MetricsSnapshot{}
	for _, stack := range sortedKeys(stacks) {
		// Every service of a stack carries the same source labels (render.go).
		s := stacks[stack][0]
		m.Sources = append(m.Sources, StackSource{Stack: stack, Repo: s.Label(render.LabelRepo), Env: s.Label(render.LabelEnv),
			Branch: s.Label(render.LabelRef), Commit: s.Label(render.LabelCommit)})
		counts, err := c.docker.ReplicaCounts(ctx, stack)
		if err != nil {
			return nil, err
		}
		for _, name := range sortedKeys(counts) {
			rc := counts[name]
			m.Stacks = append(m.Stacks, StackReplicas{Stack: stack, Service: name, Desired: rc.Desired, Running: rc.Running})
		}
	}
	c.st.View(func(d *state.Data) {
		m.LastScan = d.LastScan
		m.LastScanError = d.LastScanError != ""
		m.LastImageCheck = d.LastImageCheck
		m.Orphaned = len(d.Orphaned)
		m.PruneBlocked = len(d.PruneBlocked)
	})
	return m, nil
}
