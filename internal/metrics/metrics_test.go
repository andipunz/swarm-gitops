package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andipunz/swarm-gitops/internal/controller"
)

func TestWrite(t *testing.T) {
	m := &controller.MetricsSnapshot{
		Stacks: []controller.StackReplicas{
			{Stack: "app-prod", Service: "app-prod_web", Desired: 3, Running: 2},
			{Stack: "app-prod", Service: "app-prod_worker", Desired: -1, Running: 1}, // global service
		},
		Sources:        []controller.StackSource{{Stack: "app-prod", Repo: "app", Env: "prod", Branch: "main", Commit: "abc123"}},
		LastScan:       time.Unix(1700000000, 0),
		LastScanError:  true,
		LastImageCheck: time.Unix(1700000100, 0),
		Orphaned:       2,
		PruneBlocked:   1,
	}
	rec := httptest.NewRecorder()
	write(rec, m)
	body := rec.Body.String()

	want := []string{
		`swarm_gitops_service_desired_replicas{stack="app-prod",service="app-prod_web"} 3`,
		`swarm_gitops_service_running_replicas{stack="app-prod",service="app-prod_web"} 2`,
		`swarm_gitops_service_running_replicas{stack="app-prod",service="app-prod_worker"} 1`,
		`swarm_gitops_stack_info{stack="app-prod",repo="app",env="prod",branch="main",commit="abc123"} 1`,
		"swarm_gitops_last_scan_timestamp_seconds 1700000000",
		"swarm_gitops_last_scan_error 1",
		"swarm_gitops_last_image_check_timestamp_seconds 1700000100",
		"swarm_gitops_orphaned_stacks 2",
		"swarm_gitops_prune_blocked_stacks 1",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in:\n%s", w, body)
		}
	}
	// A global service (Desired == -1) must not get a desired_replicas line.
	if strings.Contains(body, `desired_replicas{stack="app-prod",service="app-prod_worker"}`) {
		t.Errorf("global service must not have a desired_replicas line:\n%s", body)
	}
}

func TestEscaping(t *testing.T) {
	m := &controller.MetricsSnapshot{
		Stacks: []controller.StackReplicas{{Stack: `weird"stack`, Service: `back\slash`, Desired: 1, Running: 1}},
	}
	rec := httptest.NewRecorder()
	write(rec, m)
	if !strings.Contains(rec.Body.String(), `stack="weird\"stack",service="back\\slash"`) {
		t.Fatalf("labels not escaped:\n%s", rec.Body.String())
	}
}
