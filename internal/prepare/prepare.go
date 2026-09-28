// Package prepare creates and verifies bind mount folders on a node. It runs
// inside a short-lived Swarm job (mode: global-job) that has the static part
// of each bind rule (e.g. /srv/swarm) mounted at /host/srv/swarm.
package prepare

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

// HostRoot is where the job sees the host folders.
const HostRoot = "/host"

// Spec is one folder to prepare.
type Spec struct {
	Prefix string // static folder that must already exist on the node, e.g. /srv/swarm
	Path   string // host path to verify/create, below Prefix
	Create bool
}

// String encodes a spec as a command line argument: create|prefix|path.
func (s Spec) String() string {
	mode := "check"
	if s.Create {
		mode = "create"
	}
	return mode + "|" + s.Prefix + "|" + s.Path
}

// ParseSpec decodes String().
func ParseSpec(arg string) (Spec, error) {
	parts := strings.SplitN(arg, "|", 3)
	if len(parts) != 3 || (parts[0] != "create" && parts[0] != "check") {
		return Spec{}, fmt.Errorf("invalid spec %q", arg)
	}
	return Spec{Create: parts[0] == "create", Prefix: path.Clean(parts[1]), Path: path.Clean(parts[2])}, nil
}

// Run prepares all specs below root (HostRoot in the job, a temp dir in tests).
// Every component below the prefix must be a real directory: symlinks are
// refused, because Docker follows them when it bind-mounts a source.
func Run(root string, specs []Spec, owner string) error {
	uid, gid := -1, -1
	if owner != "" {
		u, g, ok := strings.Cut(owner, ":")
		var err1, err2 error
		uid, err1 = strconv.Atoi(u)
		gid, err2 = strconv.Atoi(g)
		if !ok || err1 != nil || err2 != nil {
			return fmt.Errorf("invalid owner %q", owner)
		}
	}
	var errs []error
	for _, s := range specs {
		if err := one(root, s, uid, gid); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Path, err))
		}
	}
	return errors.Join(errs...)
}

func one(root string, s Spec, uid, gid int) error {
	if s.Path != s.Prefix && !strings.HasPrefix(s.Path, strings.TrimSuffix(s.Prefix, "/")+"/") {
		return fmt.Errorf("not below %s", s.Prefix)
	}
	base := root + s.Prefix
	fi, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("base folder %s does not exist on this node (an admin must create it once)", s.Prefix)
	}
	if !fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("base folder %s is not a directory", s.Prefix)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(s.Path, s.Prefix), "/")
	cur := base
	shown := s.Prefix
	if rel == "" {
		return nil
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path component %q", part)
		}
		cur += "/" + part
		shown = path.Join(shown, part)
		last := i == len(parts)-1
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink, refusing to mount it", shown)
		case err == nil && !fi.IsDir() && !last:
			return fmt.Errorf("%s exists but is not a directory", shown)
		case err == nil:
			// The leaf may be a non-directory (e.g. a Unix socket like
			// /var/run/docker.sock) - Docker can bind-mount a file just as
			// well as a directory. Only existence and "not a symlink" are
			// verified for it; it's never created or chowned (Create only
			// ever makes directories, never a substitute for a missing
			// file/socket - see below).
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return err
		case !s.Create:
			return fmt.Errorf("%s does not exist (this folder is not created automatically)", shown)
		}
		if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if uid >= 0 {
			if err := os.Lchown(cur, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}
