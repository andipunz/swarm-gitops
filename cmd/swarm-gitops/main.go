// swarm-gitops: pull-based GitOps for Docker Swarm, driven by GitHub.
//
//	swarm-gitops serve                 run the controller (in the Swarm service)
//	swarm-gitops status                overview of managed stacks
//	swarm-gitops sync                  scan now
//	swarm-gitops redeploy <stack>      force a redeploy of the current commit
//	swarm-gitops pause|resume <stack>  stop/start deploys, image updates and removal
//	swarm-gitops adopt <stack>         allow taking over an existing unmanaged stack
//	swarm-gitops approve-prune         allow a blocked mass removal once
//	swarm-gitops reset-binds <folder>  forget the writable-mount history of a bind folder
//	swarm-gitops prepare <specs>       (internal) create/verify bind folders on a node
//	swarm-gitops check [dir]           validate .swarm/ locally or in CI (needs docker CLI)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/andipunz/swarm-gitops/internal/api"
	"github.com/andipunz/swarm-gitops/internal/config"
	"github.com/andipunz/swarm-gitops/internal/controller"
	"github.com/andipunz/swarm-gitops/internal/gh"
	"github.com/andipunz/swarm-gitops/internal/metrics"
	"github.com/andipunz/swarm-gitops/internal/policy"
	"github.com/andipunz/swarm-gitops/internal/prepare"
	"github.com/andipunz/swarm-gitops/internal/registry"
	"github.com/andipunz/swarm-gitops/internal/render"
	"github.com/andipunz/swarm-gitops/internal/spec"
	"github.com/andipunz/swarm-gitops/internal/state"
	"github.com/andipunz/swarm-gitops/internal/swarm"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	socket := os.Getenv("SOCKET")
	if socket == "" {
		socket = "/run/swarm-gitops/api.sock"
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve()
	case "status":
		err = status(socket)
	case "sync":
		err = simple(socket, "/v1/sync")
	case "approve-prune":
		err = simple(socket, "/v1/approve-prune")
	case "redeploy", "pause", "resume", "adopt":
		if len(args) != 1 {
			usage()
		}
		err = simple(socket, "/v1/"+cmd+"?stack="+args[0])
	case "reset-binds":
		if len(args) != 1 {
			usage()
		}
		err = simple(socket, "/v1/reset-binds?root="+url.QueryEscape(args[0]))
	case "check":
		err = check(args)
	case "prepare":
		err = prepareCmd(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: swarm-gitops <command>
  serve                 run the controller
  status                show managed stacks
  sync                  scan now
  redeploy <stack>      force redeploy of the current commit
  pause <stack>         stop deploys, image updates and removal for a stack
  resume <stack>        undo pause
  adopt <stack>         allow taking over an existing unmanaged stack
  approve-prune         allow a blocked mass removal once
  reset-binds <folder>  forget the writable-mount history of a bind folder (after checking it)
  check [-env e] [-policy f] [dir]   validate a .swarm directory (default ./.swarm)`)
	os.Exit(2)
}

func serve() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level()}))
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	var client *gh.Client
	if cfg.Token != "" {
		client = gh.NewWithToken(cfg.Org, cfg.APIURL, cfg.Token)
	} else {
		key, err := os.ReadFile(cfg.AppKeyFile)
		if err != nil {
			return err
		}
		if client, err = gh.New(cfg.Org, cfg.APIURL, cfg.AppID, key); err != nil {
			return err
		}
	}
	pol, err := policy.Load(cfg.PolicyFile)
	if err != nil {
		return err
	}
	st, err := state.Open(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		return err
	}
	dockerEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + cfg.DataDir}
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if v := os.Getenv(k); v != "" {
			dockerEnv = append(dockerEnv, k+"="+v)
		}
	}
	docker := &swarm.Docker{Env: dockerEnv, DryRun: cfg.DryRun}
	regCfg := ""
	if cfg.DockerConfig != "" {
		regCfg = filepath.Join(cfg.DockerConfig, "config.json")
	}
	ctrl := controller.New(cfg, client, docker, registry.NewResolver(regCfg), pol, st, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := api.Serve(ctx, cfg.Socket, ctrl); err != nil {
			log.Error("admin socket", "err", err)
		}
	}()
	if cfg.MetricsAddr != "" {
		go func() {
			if err := metrics.Serve(ctx, cfg.MetricsAddr, ctrl); err != nil {
				log.Error("metrics listener", "err", err)
			}
		}()
	}
	log.Info("swarm-gitops started", "org", cfg.Org, "scan", cfg.ScanInterval.String(), "images", cfg.ImageInterval.String(), "dry_run", cfg.DryRun)
	ctrl.Run(ctx)
	return nil
}

func level() slog.Level {
	if os.Getenv("LOG_LEVEL") == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func simple(socket, path string) error {
	var out map[string]string
	if err := api.Call(socket, "POST", path, &out); err != nil {
		return err
	}
	fmt.Println(out["result"])
	return nil
}

func status(socket string) error {
	var st controller.Status
	if err := api.Call(socket, "GET", "/v1/status", &st); err != nil {
		return err
	}
	if len(os.Args) > 2 && os.Args[2] == "--json" {
		return json.NewEncoder(os.Stdout).Encode(st)
	}
	fmt.Printf("last scan: %s", st.LastScan.Format(time.RFC3339))
	if st.LastScanError != "" {
		fmt.Printf("  ERROR: %s", st.LastScanError)
	}
	fmt.Printf("\nlast image check: %s\n", st.LastImageCheck.Format(time.RFC3339))
	if st.DryRun {
		fmt.Println("DRY RUN: nothing is deployed or removed")
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STACK\tREPO\tENV\tREF\tCOMMIT\tSERVICES\tNOTE")
	for _, s := range st.Stacks {
		var note []string
		if s.Paused {
			note = append(note, "paused")
		}
		if s.PendingRemoval > 0 {
			note = append(note, fmt.Sprintf("removal pending (%d)", s.PendingRemoval))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", s.Stack, s.Repo, s.Env, s.Ref, s.Commit, s.Services, strings.Join(note, ", "))
	}
	_ = w.Flush()
	if len(st.PruneBlocked) > 0 {
		fmt.Printf("\nREMOVAL BLOCKED (run approve-prune to allow): %s\n", strings.Join(st.PruneBlocked, ", "))
	}
	for stack, why := range st.Orphaned {
		fmt.Printf("\norphaned: %s — %s", stack, why)
	}
	if len(st.Orphaned) > 0 {
		fmt.Println()
	}
	return nil
}

// check validates a .swarm directory exactly like the controller would.
func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	envName := fs.String("env", "", "only check this environment")
	policyFile := fs.String("policy", "", "policy file (default: built-in policy)")
	repo := fs.String("repo", "", "repository name (default: parent directory name)")
	_ = fs.Parse(args)
	dir := ".swarm"
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if *repo == "" {
		*repo = filepath.Base(filepath.Dir(abs))
	}
	data, err := os.ReadFile(filepath.Join(abs, "deploy.yml"))
	if err != nil {
		return err
	}
	f, err := spec.Parse(data)
	if err != nil {
		return err
	}
	pol, err := policy.Load(*policyFile)
	if err != nil {
		return err
	}
	failed := false
	for _, env := range f.EnvNames() {
		if *envName != "" && env != *envName {
			continue
		}
		e := f.Environments[env]
		branch := e.Ref
		if e.IsGlob() {
			branch = strings.NewReplacer("*", "example", "?", "x").Replace(e.Ref)
		}
		tmp, err := os.MkdirTemp("", "swarm-check-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		if err := os.CopyFS(tmp, os.DirFS(abs)); err != nil {
			return err
		}
		files, optional := e.ComposeFiles(env)
		stack := spec.StackName(*repo, env, e, branch)
		res, err := render.Render(context.Background(), render.Input{
			Stack: stack, Repo: *repo, Env: env, GitHubEnv: spec.GitHubEnvironment(env, e, branch), Ref: branch,
			Commit: "0000000000000000000000000000000000000000", WorkDir: tmp, Files: files, Optional: optional,
			VarsFile: e.Vars, Policy: pol,
		})
		switch {
		case err != nil:
			failed = true
			fmt.Printf("✗ %s (%s): %v\n", env, stack, err)
		case len(res.Violations) > 0:
			failed = true
			fmt.Printf("✗ %s (%s): %d policy violation(s)\n", env, stack, len(res.Violations))
			for _, v := range res.Violations {
				fmt.Println("    -", v)
			}
		default:
			fmt.Printf("✓ %s → stack %s (%s), services: %s\n", env, stack, e.Ref, strings.Join(res.Services, ", "))
		}
	}
	if failed {
		return fmt.Errorf("check failed")
	}
	return nil
}

// prepareCmd runs inside the per-node job: create/verify bind folders.
func prepareCmd(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	owner := fs.String("owner", "", "uid:gid for created folders")
	_ = fs.Parse(args)
	var specs []prepare.Spec
	for _, a := range fs.Args() {
		s, err := prepare.ParseSpec(a)
		if err != nil {
			return err
		}
		specs = append(specs, s)
	}
	if err := prepare.Run(prepare.HostRoot, specs, *owner); err != nil {
		return err
	}
	fmt.Printf("prepared %d folder(s)\n", len(specs))
	return nil
}
