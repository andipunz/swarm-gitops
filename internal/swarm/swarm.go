// Package swarm wraps the docker CLI (stack deploy/rm, service inspect/ps/logs).
// Using the CLI keeps behaviour identical to what admins run by hand.
package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/bergwacht-bayern/swarm-gitops/internal/render"
)

// Service is the subset of `docker service inspect` we use.
type Service struct {
	ID   string `json:"ID"`
	Spec struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
		Mode   struct {
			Replicated *struct {
				Replicas *uint64 `json:"Replicas"`
			} `json:"Replicated"`
			Global *struct{} `json:"Global"`
		} `json:"Mode"`
		TaskTemplate struct {
			ContainerSpec struct {
				Image   string `json:"Image"`
				Configs []struct {
					ConfigName string `json:"ConfigName"`
				} `json:"Configs"`
			} `json:"ContainerSpec"`
		} `json:"TaskTemplate"`
	} `json:"Spec"`
	PreviousSpec *struct {
		TaskTemplate struct {
			ContainerSpec struct {
				Configs []struct {
					ConfigName string `json:"ConfigName"`
				} `json:"Configs"`
			} `json:"ContainerSpec"`
		} `json:"TaskTemplate"`
	} `json:"PreviousSpec"`
	UpdateStatus *struct {
		State   string `json:"State"`
		Message string `json:"Message"`
	} `json:"UpdateStatus"`
}

// Label returns a spec label.
func (s Service) Label(k string) string { return s.Spec.Labels[k] }

// Namespace is the stack a service belongs to.
func (s Service) Namespace() string { return s.Spec.Labels["com.docker.stack.namespace"] }

// Docker runs docker CLI commands. Env is passed to every command
// (DOCKER_HOST, DOCKER_CONFIG for registry auth, ...).
type Docker struct {
	Env    []string
	DryRun bool
}

func (d *Docker) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = d.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("docker %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (d *Docker) mutate(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	if d.DryRun {
		return nil, nil
	}
	return d.run(ctx, timeout, args...)
}

// Services returns services matching a label filter (e.g. "swarm-gitops.managed=true").
func (d *Docker) Services(ctx context.Context, filter string) ([]Service, error) {
	out, err := d.run(ctx, 30*time.Second, "service", "ls", "-q", "--filter", "label="+filter)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = d.run(ctx, 60*time.Second, append([]string{"service", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var svcs []Service
	if err := json.Unmarshal(out, &svcs); err != nil {
		return nil, err
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Spec.Name < svcs[j].Spec.Name })
	return svcs, nil
}

// ManagedStacks groups all controller-managed services by stack.
func (d *Docker) ManagedStacks(ctx context.Context) (map[string][]Service, error) {
	svcs, err := d.Services(ctx, render.LabelManaged+"=true")
	if err != nil {
		return nil, err
	}
	res := map[string][]Service{}
	for _, s := range svcs {
		res[s.Label(render.LabelStack)] = append(res[s.Label(render.LabelStack)], s)
	}
	return res, nil
}

// StackServices returns all services of a stack, managed or not.
func (d *Docker) StackServices(ctx context.Context, stack string) ([]Service, error) {
	return d.Services(ctx, "com.docker.stack.namespace="+stack)
}

// Deploy runs docker stack deploy with --prune (services removed from the file are removed).
func (d *Docker) Deploy(ctx context.Context, stack, file string) error {
	_, err := d.mutate(ctx, 5*time.Minute, "stack", "deploy", "--prune", "--with-registry-auth", "--resolve-image", "always", "--detach=true", "-c", file, stack)
	return err
}

// Remove removes a stack. Named volumes stay on the nodes.
func (d *Docker) Remove(ctx context.Context, stack string) error {
	_, err := d.mutate(ctx, 2*time.Minute, "stack", "rm", stack)
	return err
}

// ForceUpdate restarts all tasks of a service (used for empty-commit redeploys).
func (d *Docker) ForceUpdate(ctx context.Context, service string) error {
	_, err := d.mutate(ctx, time.Minute, "service", "update", "--force", "--detach", "--with-registry-auth", service)
	return err
}

// UpdateImage points a service to a new image digest.
func (d *Docker) UpdateImage(ctx context.Context, service, image string) error {
	_, err := d.mutate(ctx, time.Minute, "service", "update", "--detach", "--with-registry-auth", "--image", image, service)
	return err
}

// Task is a line of `docker service ps --format json`.
type Task struct {
	Name         string `json:"Name"`
	Node         string `json:"Node"`
	DesiredState string `json:"DesiredState"`
	CurrentState string `json:"CurrentState"`
	Error        string `json:"Error"`
}

// Tasks lists the tasks of a service (newest first, as docker prints them).
func (d *Docker) Tasks(ctx context.Context, service string) ([]Task, error) {
	out, err := d.run(ctx, 30*time.Second, "service", "ps", "--no-trunc", "--format", "json", service)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var t Task
		if err := json.Unmarshal([]byte(line), &t); err == nil {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// Logs returns the last lines of a service's logs.
func (d *Docker) Logs(ctx context.Context, service string, lines int) string {
	out, err := d.run(ctx, 15*time.Second, "service", "logs", "--no-task-ids", "--tail", fmt.Sprint(lines), service)
	if err != nil {
		return err.Error()
	}
	return string(out)
}

// PruneConfigs removes configs of a stack no longer referenced by any
// service spec or previous spec (so rollbacks keep working).
func (d *Docker) PruneConfigs(ctx context.Context, stack string) {
	out, err := d.run(ctx, 30*time.Second, "config", "ls", "--filter", "label=com.docker.stack.namespace="+stack, "--format", "{{.Name}}")
	if err != nil {
		return
	}
	svcs, err := d.StackServices(ctx, stack)
	if err != nil {
		return
	}
	used := map[string]bool{}
	for _, s := range svcs {
		for _, c := range s.Spec.TaskTemplate.ContainerSpec.Configs {
			used[c.ConfigName] = true
		}
		if s.PreviousSpec != nil {
			for _, c := range s.PreviousSpec.TaskTemplate.ContainerSpec.Configs {
				used[c.ConfigName] = true
			}
		}
	}
	for _, name := range strings.Fields(string(out)) {
		if !used[name] {
			_, _ = d.mutate(ctx, 30*time.Second, "config", "rm", name)
		}
	}
}

// ServiceReport is the rollout outcome of one service.
type ServiceReport struct {
	Name    string
	Image   string
	Running int
	Desired int // -1 for global services
	State   string
	Error   string
	Logs    string
}

// WaitRollout polls until every service of the stack is converged, failed or
// the timeout expires. ok is false if any service failed or timed out.
func (d *Docker) WaitRollout(ctx context.Context, stack string, timeout time.Duration) (ok bool, reports []ServiceReport, err error) {
	if d.DryRun {
		return true, nil, nil
	}
	deadline := time.Now().Add(timeout)
	for {
		svcs, err := d.StackServices(ctx, stack)
		if err != nil {
			return false, nil, err
		}
		reports = reports[:0]
		done, failed := true, false
		for _, s := range svcs {
			r := ServiceReport{Name: s.Spec.Name, Image: s.Spec.TaskTemplate.ContainerSpec.Image, Desired: -1}
			if s.Spec.Mode.Replicated != nil && s.Spec.Mode.Replicated.Replicas != nil {
				r.Desired = int(*s.Spec.Mode.Replicated.Replicas)
			}
			tasks, err := d.Tasks(ctx, s.ID)
			if err != nil {
				return false, nil, err
			}
			desiredRunning := 0
			for _, t := range tasks {
				if t.DesiredState == "Running" {
					desiredRunning++
					if strings.HasPrefix(t.CurrentState, "Running") {
						r.Running++
					} else if t.Error != "" && r.Error == "" {
						r.Error = t.Error
					}
				} else if t.Error != "" && r.Error == "" {
					r.Error = t.Error
				}
			}
			want := r.Desired
			if want < 0 {
				want = desiredRunning
			}
			switch {
			case s.UpdateStatus != nil && (s.UpdateStatus.State == "paused" || strings.HasPrefix(s.UpdateStatus.State, "rollback")):
				if s.UpdateStatus.State == "rollback_started" {
					r.State = "rolling back"
					done = false
				} else {
					r.State = s.UpdateStatus.State
					failed = true
				}
				if r.Error == "" {
					r.Error = s.UpdateStatus.Message
				}
			case s.UpdateStatus != nil && s.UpdateStatus.State == "updating":
				r.State = "updating"
				done = false
			case r.Running >= want:
				r.State = "running"
			default:
				r.State = "starting"
				done = false
			}
			if r.State == "running" {
				r.Error = "" // errors of old, already replaced tasks are noise
			}
			reports = append(reports, r)
		}
		if failed || done {
			ok = !failed
			if !ok || !done {
				d.attachLogs(ctx, reports)
			}
			return ok, reports, nil
		}
		if time.Now().After(deadline) {
			for i := range reports {
				if reports[i].State != "running" {
					reports[i].State += " (timed out)"
				}
			}
			d.attachLogs(ctx, reports)
			return false, reports, nil
		}
		select {
		case <-ctx.Done():
			return false, reports, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (d *Docker) attachLogs(ctx context.Context, reports []ServiceReport) {
	for i := range reports {
		if reports[i].State != "running" {
			reports[i].Logs = d.Logs(ctx, reports[i].Name, 30)
		}
	}
}
