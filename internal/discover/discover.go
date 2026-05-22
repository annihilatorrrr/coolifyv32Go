// Package discover finds a running coolify v3 stack on the local Docker daemon
// and extracts the bits the migrater needs without the user supplying them:
//   - the COOLIFY_SECRET_KEY (read from the `coolify` container's env)
//   - the path to v3's SQLite file (`/app/db/prod.db` inside the container,
//     extracted via docker cp to a local temp dir)
//   - the runtime config of every v3-managed app and DB container, indexed
//     by the v3 row id from container labels / env.
package discover

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// V3Stack is the result of inspecting a running v3 install.
type V3Stack struct {
	CoolifyContainerID string
	FluentBitID        string
	TraefikID          string
	SQLitePath         string // local filesystem; caller must remove when done
	SecretKey          string // raw COOLIFY_SECRET_KEY from coolify env
	NetworkID          string // coolify-infra
	NetworkName        string
	WorkloadContainers []V3Workload
	Volumes            []string // v3-stack volumes (coolify-db, coolify-logs, ...)
}

// V3Workload is a single user-owned container managed by v3 — either an app
// container or a database container. Identified by the v3 row id discovered
// either via the container name (v3 uses `<name>-<id>` for apps) or a heuristic
// fallback on labels/env (v3 doesn't set as clean a label set as we'd like).
type V3Workload struct {
	Labels        map[string]string
	PortBindings  map[int]int // host port → container port; populated from Docker HostConfig
	ContainerID   string
	ContainerName string
	Image         string
	Networks      []string
	Volumes       []container.MountPoint
	Env           []string
	Running       bool
}

// DefaultSQLitePath is the path inside the coolify container.
const DefaultSQLitePath = "/app/db/prod.db"

// Inspect discovers the v3 stack on the local Docker daemon.
func Inspect(ctx context.Context, dc *client.Client) (*V3Stack, error) {
	stack := &V3Stack{}

	args := filters.NewArgs()
	args.Add("name", "coolify")
	containers, err := dc.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list coolify containers: %w", err)
	}
	for _, c := range containers {
		name := stripSlash(c.Names)
		switch {
		case name == "coolify":
			stack.CoolifyContainerID = c.ID
		case name == "coolify-fluentbit":
			stack.FluentBitID = c.ID
		case strings.HasPrefix(name, "coolify-proxy") || name == "coolify-haproxy":
			stack.TraefikID = c.ID
		}
	}
	if stack.CoolifyContainerID == "" {
		return nil, errors.New("no `coolify` container found — is v3 installed on this host")
	}

	insp, err := dc.ContainerInspect(ctx, stack.CoolifyContainerID)
	if err != nil {
		return nil, fmt.Errorf("inspect coolify: %w", err)
	}
	for _, env := range insp.Config.Env {
		if k, v, ok := strings.Cut(env, "="); ok {
			if k == "COOLIFY_SECRET_KEY_BETTER" && v != "" {
				stack.SecretKey = v
			}
			if k == "COOLIFY_SECRET_KEY" && stack.SecretKey == "" {
				stack.SecretKey = v
			}
		}
	}
	if stack.SecretKey == "" {
		return nil, errors.New("COOLIFY_SECRET_KEY not found in coolify container env — supply --v3-secret-key")
	}

	// Network — v3 calls it coolify-infra. Cache the id so teardown can remove it.
	nets, err := dc.NetworkList(ctx, network.ListOptions{})
	if err == nil {
		for _, n := range nets {
			if n.Name == "coolify-infra" {
				stack.NetworkID = n.ID
				stack.NetworkName = n.Name
			}
		}
	}

	// All workload containers — every container connected to coolify-infra
	// that isn't part of v3's own stack is a user workload.
	wargs := filters.NewArgs()
	wargs.Add("network", "coolify-infra")
	wlist, err := dc.ContainerList(ctx, container.ListOptions{All: true, Filters: wargs})
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}
	stackIDs := map[string]bool{
		stack.CoolifyContainerID: true,
		stack.FluentBitID:        true,
		stack.TraefikID:          true,
	}
	for _, c := range wlist {
		if stackIDs[c.ID] {
			continue
		}
		full, ierr := dc.ContainerInspect(ctx, c.ID)
		if ierr != nil {
			continue
		}
		w := V3Workload{
			ContainerID:   c.ID,
			ContainerName: stripSlash(c.Names),
			Image:         c.Image,
			Env:           full.Config.Env,
			Labels:        full.Config.Labels,
			Running:       full.State != nil && full.State.Running,
			Volumes:       full.Mounts,
			PortBindings:  extractPortBindings(full.HostConfig),
		}
		for net := range full.NetworkSettings.Networks {
			w.Networks = append(w.Networks, net)
		}
		stack.WorkloadContainers = append(stack.WorkloadContainers, w)
	}

	// Volumes — v3 docker-compose names six (coolify-db, coolify-logs,
	// coolify-local-backup, coolify-ssl-certs, coolify-traefik-letsencrypt,
	// coolify-letsencrypt, optionally coolify-pgdb).
	for _, name := range []string{
		"coolify-db", "coolify-logs", "coolify-local-backup",
		"coolify-ssl-certs", "coolify-traefik-letsencrypt", "coolify-letsencrypt",
		"coolify-pgdb",
	} {
		if _, vErr := dc.VolumeInspect(ctx, name); vErr == nil {
			stack.Volumes = append(stack.Volumes, name)
		}
	}

	return stack, nil
}

// ExtractSQLite copies /app/db/prod.db out of the `coolify` container to a
// temp dir on the host. v3 must be stopped before calling (otherwise the file
// may be mid-write). Returns the local path; caller deletes when done.
func ExtractSQLite(ctx context.Context, dc *client.Client, coolifyID string) (string, error) {
	rc, _, err := dc.CopyFromContainer(ctx, coolifyID, DefaultSQLitePath)
	if err != nil {
		return "", fmt.Errorf("copy %s: %w", DefaultSQLitePath, err)
	}
	defer rc.Close()

	tmpDir, err := os.MkdirTemp("", "coolfymigrater-v3db-*")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(tmpDir, "prod.db")

	tr := tar.NewReader(rc)
	for {
		hdr, terr := tr.Next()
		if errors.Is(terr, io.EOF) {
			break
		}
		if terr != nil {
			return "", fmt.Errorf("untar: %w", terr)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		f, ferr := os.Create(dst)
		if ferr != nil {
			return "", ferr
		}
		if _, terr = io.Copy(f, tr); terr != nil {
			_ = f.Close()
			return "", terr
		}
		_ = f.Close()
	}
	return dst, nil
}

// extractPortBindings reads HostConfig.PortBindings and returns a map of
// hostPort → containerPort for every published port.
func extractPortBindings(hc *container.HostConfig) map[int]int {
	if hc == nil || len(hc.PortBindings) == 0 {
		return nil
	}
	out := make(map[int]int, len(hc.PortBindings))
	for containerPort, bindings := range hc.PortBindings {
		cp := containerPort.Int()
		for _, b := range bindings {
			hp, err := strconv.Atoi(b.HostPort)
			if err != nil || hp == 0 {
				continue
			}
			out[hp] = cp
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stripSlash(names []string) string {
	if len(names) == 0 {
		return ""
	}
	n := names[0]
	if strings.HasPrefix(n, "/") {
		return n[1:]
	}
	return n
}
