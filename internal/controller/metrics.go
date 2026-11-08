package controller

import (
	"context"
	"time"

	"github.com/andipunz/swarm-gitops/internal/registry"
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

// ServiceImage is the image a service runs and, from its OCI labels, the
// commit it was built from - for the /metrics endpoint. A stack's source
// commit only says where its .swarm/ came from; new images of the same tag
// roll out without changing it.
type ServiceImage struct {
	Stack, Service, Image     string
	Revision, Source, Version string
}

// MetricsSnapshot is the point-in-time data exposed at /metrics.
type MetricsSnapshot struct {
	Stacks         []StackReplicas
	Sources        []StackSource
	Images         []ServiceImage
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
// Image labels come from the registry, cached per digest.
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
		for _, svc := range stacks[stack] {
			m.Images = append(m.Images, c.serviceImage(stack, svc.Spec.Name, svc.Spec.TaskTemplate.ContainerSpec.Image))
		}
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

// serviceImage reads the OCI labels of a service's pinned image digest. Images
// without a digest (never resolved, e.g. local-only), whose lookup failed or
// is still running (a new digest: next scrape) get no labels - the image
// itself is still reported.
func (c *Controller) serviceImage(stack, service, image string) ServiceImage {
	si := ServiceImage{Stack: stack, Service: service, Image: image}
	ref := registry.Parse(image)
	if ref.Digest == "" || c.reg == nil {
		return si
	}
	labels, ok := c.reg.Labels(ref)
	if !ok {
		return si
	}
	si.Revision = labels["org.opencontainers.image.revision"]
	si.Source = labels["org.opencontainers.image.source"]
	si.Version = labels["org.opencontainers.image.version"]
	return si
}
