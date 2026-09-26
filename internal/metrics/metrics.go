// Package metrics exposes a read-only Prometheus endpoint on its own TCP
// listener, deliberately separate from the admin API (which stays a
// Unix-socket, never network-reachable, on purpose). Every value it reports
// comes from data the controller already computes for its own reconcile
// loop and admin status output - this endpoint costs no new Docker
// privilege, only a new place to read numbers that already exist.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andipunz/swarm-gitops/internal/controller"
)

// Serve listens on addr until ctx is done.
func Serve(ctx context.Context, addr string, c *controller.Controller) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		reqCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		m, err := c.Metrics(reqCtx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		write(w, m)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func write(w http.ResponseWriter, m *controller.MetricsSnapshot) {
	fmt.Fprintln(w, "# HELP swarm_gitops_service_desired_replicas Desired replica count (replicated services only, not global).")
	fmt.Fprintln(w, "# TYPE swarm_gitops_service_desired_replicas gauge")
	for _, s := range m.Stacks {
		if s.Desired >= 0 {
			fmt.Fprintf(w, "swarm_gitops_service_desired_replicas{stack=\"%s\",service=\"%s\"} %d\n", esc(s.Stack), esc(s.Service), s.Desired)
		}
	}
	fmt.Fprintln(w, "# HELP swarm_gitops_service_running_replicas Currently running replica count.")
	fmt.Fprintln(w, "# TYPE swarm_gitops_service_running_replicas gauge")
	for _, s := range m.Stacks {
		fmt.Fprintf(w, "swarm_gitops_service_running_replicas{stack=\"%s\",service=\"%s\"} %d\n", esc(s.Stack), esc(s.Service), s.Running)
	}
	fmt.Fprintln(w, "# HELP swarm_gitops_last_scan_timestamp_seconds Unix time the last scan completed.")
	fmt.Fprintln(w, "# TYPE swarm_gitops_last_scan_timestamp_seconds gauge")
	fmt.Fprintf(w, "swarm_gitops_last_scan_timestamp_seconds %d\n", m.LastScan.Unix())
	fmt.Fprintln(w, "# HELP swarm_gitops_last_scan_error 1 if the last scan failed (nothing was changed that scan).")
	fmt.Fprintln(w, "# TYPE swarm_gitops_last_scan_error gauge")
	fmt.Fprintf(w, "swarm_gitops_last_scan_error %d\n", boolInt(m.LastScanError))
	fmt.Fprintln(w, "# HELP swarm_gitops_last_image_check_timestamp_seconds Unix time of the last automatic image-update check.")
	fmt.Fprintln(w, "# TYPE swarm_gitops_last_image_check_timestamp_seconds gauge")
	fmt.Fprintf(w, "swarm_gitops_last_image_check_timestamp_seconds %d\n", m.LastImageCheck.Unix())
	fmt.Fprintln(w, "# HELP swarm_gitops_orphaned_stacks Stacks kept because their source repository is no longer visible.")
	fmt.Fprintln(w, "# TYPE swarm_gitops_orphaned_stacks gauge")
	fmt.Fprintf(w, "swarm_gitops_orphaned_stacks %d\n", m.Orphaned)
	fmt.Fprintln(w, "# HELP swarm_gitops_prune_blocked_stacks Removals waiting for admin approval (approve-prune).")
	fmt.Fprintln(w, "# TYPE swarm_gitops_prune_blocked_stacks gauge")
	fmt.Fprintf(w, "swarm_gitops_prune_blocked_stacks %d\n", m.PruneBlocked)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func esc(s string) string { return labelEscaper.Replace(s) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
