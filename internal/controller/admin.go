package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/andipunz/swarm-gitops/internal/render"
	"github.com/andipunz/swarm-gitops/internal/state"
)

// StackStatus is one line of `swarm-gitops status`.
type StackStatus struct {
	Stack          string `json:"stack"`
	Repo           string `json:"repo"`
	Env            string `json:"env"`
	Ref            string `json:"ref"`
	Commit         string `json:"commit"`
	Services       int    `json:"services"`
	Paused         bool   `json:"paused,omitempty"`
	PendingRemoval int    `json:"pending_removal,omitempty"`
}

// Status is the admin overview.
type Status struct {
	LastScan       time.Time         `json:"last_scan"`
	LastScanError  string            `json:"last_scan_error,omitempty"`
	LastImageCheck time.Time         `json:"last_image_check"`
	DryRun         bool              `json:"dry_run,omitempty"`
	Stacks         []StackStatus     `json:"stacks"`
	Orphaned       map[string]string `json:"orphaned,omitempty"`
	PruneBlocked   []string          `json:"prune_blocked,omitempty"`
}

// Status collects the current state.
func (c *Controller) Status(ctx context.Context) (*Status, error) {
	stacks, err := c.docker.ManagedStacks(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{DryRun: c.cfg.DryRun}
	c.st.View(func(d *state.Data) {
		st.LastScan, st.LastScanError, st.LastImageCheck = d.LastScan, d.LastScanError, d.LastImageCheck
		st.Orphaned, st.PruneBlocked = d.Orphaned, d.PruneBlocked
		for name, svcs := range stacks {
			s := svcs[0]
			st.Stacks = append(st.Stacks, StackStatus{
				Stack: name, Repo: s.Label(render.LabelRepo), Env: s.Label(render.LabelEnv), Ref: s.Label(render.LabelRef),
				Commit: short(s.Label(render.LabelCommit)), Services: len(svcs),
				Paused: d.Paused[name], PendingRemoval: d.PendingRemoval[name],
			})
		}
	})
	sort.Slice(st.Stacks, func(i, j int) bool { return st.Stacks[i].Stack < st.Stacks[j].Stack })
	return st, nil
}

func (c *Controller) requireManaged(ctx context.Context, stack string) error {
	stacks, err := c.docker.ManagedStacks(ctx)
	if err != nil {
		return err
	}
	if _, ok := stacks[stack]; !ok {
		return fmt.Errorf("stack %q is not managed by swarm-gitops", stack)
	}
	return nil
}

// Redeploy forces a redeploy of a stack's current commit on the next scan.
func (c *Controller) Redeploy(ctx context.Context, stack string) error {
	if err := c.requireManaged(ctx, stack); err != nil {
		return err
	}
	err := c.st.Update(func(d *state.Data) { d.Force[stack] = true })
	c.Trigger()
	return err
}

// SetPaused pauses or resumes deploys, image updates and removal of a stack.
func (c *Controller) SetPaused(ctx context.Context, stack string, paused bool) error {
	if err := c.requireManaged(ctx, stack); err != nil {
		return err
	}
	return c.st.Update(func(d *state.Data) {
		if paused {
			d.Paused[stack] = true
		} else {
			delete(d.Paused, stack)
			delete(d.Processed, stack) // re-evaluate the current commit
		}
	})
}

// Adopt allows the controller to take over an existing, unmanaged stack
// (e.g. one deployed via Portainer) with the same name.
func (c *Controller) Adopt(stack string) error {
	err := c.st.Update(func(d *state.Data) {
		d.Adopted[stack] = true
		delete(d.Processed, stack)
	})
	c.Trigger()
	return err
}

// ApprovePrune lets the next scan remove more than MAX_PRUNE stacks once.
func (c *Controller) ApprovePrune() error {
	err := c.st.Update(func(d *state.Data) { d.PruneApproved = true })
	c.Trigger()
	return err
}

// ResetBinds forgets the writable bind history of a folder root (after an
// admin checked it for planted symlinks).
func (c *Controller) ResetBinds(root string) error {
	var found bool
	err := c.st.Update(func(d *state.Data) {
		_, found = d.BindHistory[root]
		delete(d.BindHistory, root)
	})
	if err == nil && !found {
		return fmt.Errorf("no bind history for %s", root)
	}
	c.Trigger()
	return err
}
