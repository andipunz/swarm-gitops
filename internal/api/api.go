// Package api exposes admin commands over a Unix socket (never TCP), used by
// the CLI via `docker exec <container> swarm-gitops <command>`.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/andipunz/swarm-gitops/internal/controller"
)

// Serve listens on the socket until ctx is done.
func Serve(ctx context.Context, socket string, c *controller.Controller) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(socket, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		st, err := c.Status(r.Context())
		reply(w, st, err)
	})
	mux.HandleFunc("POST /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		c.Trigger()
		reply(w, map[string]string{"result": "scan triggered"}, nil)
	})
	stackCmd := func(fn func(ctx context.Context, stack string) error, msg string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			stack := r.URL.Query().Get("stack")
			if stack == "" {
				reply(w, nil, fmt.Errorf("stack is required"))
				return
			}
			reply(w, map[string]string{"result": fmt.Sprintf(msg, stack)}, fn(r.Context(), stack))
		}
	}
	mux.HandleFunc("POST /v1/redeploy", stackCmd(c.Redeploy, "%s will be redeployed"))
	mux.HandleFunc("POST /v1/pause", stackCmd(func(ctx context.Context, s string) error { return c.SetPaused(ctx, s, true) }, "%s paused"))
	mux.HandleFunc("POST /v1/resume", stackCmd(func(ctx context.Context, s string) error { return c.SetPaused(ctx, s, false) }, "%s resumed"))
	mux.HandleFunc("POST /v1/adopt", stackCmd(func(_ context.Context, s string) error { return c.Adopt(s) }, "%s may now be taken over"))
	mux.HandleFunc("POST /v1/reset-binds", func(w http.ResponseWriter, r *http.Request) {
		root := r.URL.Query().Get("root")
		reply(w, map[string]string{"result": "bind history of " + root + " cleared"}, c.ResetBinds(root))
	})
	mux.HandleFunc("POST /v1/approve-prune", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]string{"result": "pending removals approved for the next scan"}, c.ApprovePrune())
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// Call sends a command to the running controller.
func Call(socket, method, path string, out any) error {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}
	req, err := http.NewRequest(method, "http://swarm-gitops"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("controller not reachable on %s: %w", socket, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("%s", e.Error)
	}
	return json.Unmarshal(data, out)
}
