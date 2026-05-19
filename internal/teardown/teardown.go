// Package teardown removes a v3 install from the local host once migration
// has been verified. It stops and removes v3's own stack containers, drops
// the volumes that held v3's runtime (SQLite, logs, Let's-Encrypt certs),
// prunes the coollabsio images, and tears down the coolify-infra network.
//
// User app/DB volumes are NEVER touched here — those have already been copied
// (databases) or relabelled (apps) by the takeover phase, and ownership of
// any leftover bind mounts belongs to the user.
package teardown

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// V3Volumes is the canonical set of volumes v3 declares in its
// docker-compose.yaml. Inspected one-by-one — missing volumes are ignored.
var V3Volumes = []string{
	"coolify-db",
	"coolify-logs",
	"coolify-local-backup",
	"coolify-ssl-certs",
	"coolify-traefik-letsencrypt",
	"coolify-letsencrypt",
	"coolify-pgdb",
}

// V3Containers names v3's own management containers. The migrater scans for
// these by exact name; user workloads are NEVER matched against this list.
var V3Containers = []string{
	"coolify",
	"coolify-fluentbit",
	"coolify-proxy",
	"coolify-haproxy",
}

// HostPaths lists v3 install artefacts on the host filesystem. Removed last,
// after the docker objects, so a partial failure earlier leaves the host
// state intact and re-runnable.
var HostPaths = []string{
	"/data/coolify",
	"/var/lib/coolify",
	"/etc/cron.d/coolify-default",
	"/usr/local/bin/coolify",
}

// Wipe performs the full v3 teardown. Caller has already confirmed (no
// prompting here). Returns a multi-error of non-fatal issues that didn't
// block the overall wipe.
func Wipe(ctx context.Context, dc *client.Client, w io.Writer) error {
	var nonFatal []string

	for _, name := range V3Containers {
		if id, err := containerByName(ctx, dc, name); err == nil && id != "" {
			fmt.Fprintf(w, "  stop+remove %s\n", name)
			timeout := 30
			dc.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
			if err = dc.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: false}); err != nil {
				nonFatal = append(nonFatal, fmt.Sprintf("remove %s: %s", name, err))
			}
		}
	}

	for _, vol := range V3Volumes {
		if _, err := dc.VolumeInspect(ctx, vol); err == nil {
			fmt.Fprintf(w, "  remove volume %s\n", vol)
			if err = dc.VolumeRemove(ctx, vol, true); err != nil {
				nonFatal = append(nonFatal, fmt.Sprintf("remove volume %s: %s", vol, err))
			}
		}
	}

	if err := removeImagesByPrefix(ctx, dc, w, "coollabsio/coolify"); err != nil {
		nonFatal = append(nonFatal, err.Error())
	}
	if err := removeImagesByPrefix(ctx, dc, w, "ghcr.io/coollabsio/coolify"); err != nil {
		nonFatal = append(nonFatal, err.Error())
	}
	if err := removeImagesByPrefix(ctx, dc, w, "ghcr.io/coollabsio/fluent-bit"); err != nil {
		nonFatal = append(nonFatal, err.Error())
	}

	if err := removeNetwork(ctx, dc, w, "coolify-infra"); err != nil {
		nonFatal = append(nonFatal, err.Error())
	}

	for _, p := range HostPaths {
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintf(w, "  remove host path %s\n", p)
			if err = os.RemoveAll(p); err != nil {
				nonFatal = append(nonFatal, fmt.Sprintf("remove %s: %s", p, err))
			}
		}
	}

	if len(nonFatal) > 0 {
		return fmt.Errorf("teardown finished with warnings:\n  - %s", strings.Join(nonFatal, "\n  - "))
	}
	return nil
}

func containerByName(ctx context.Context, dc *client.Client, name string) (string, error) {
	args := filters.NewArgs()
	args.Add("name", "^/"+name+"$")
	list, err := dc.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", nil
	}
	return list[0].ID, nil
}

func removeImagesByPrefix(ctx context.Context, dc *client.Client, w io.Writer, prefix string) error {
	imgs, err := dc.ImageList(ctx, image.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list images: %w", err)
	}
	for _, img := range imgs {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, prefix) {
				fmt.Fprintf(w, "  remove image %s\n", tag)
				if _, rerr := dc.ImageRemove(ctx, img.ID, image.RemoveOptions{Force: true, PruneChildren: true}); rerr != nil {
					return fmt.Errorf("remove image %s: %w", tag, rerr)
				}
				break
			}
		}
	}
	return nil
}

func removeNetwork(ctx context.Context, dc *client.Client, w io.Writer, name string) error {
	if err := dc.NetworkRemove(ctx, name); err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		// Network may already be gone — accept "not found" responses by string match.
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil
		}
		return fmt.Errorf("remove network %s: %w", name, err)
	}
	fmt.Fprintf(w, "  remove network %s\n", name)
	return nil
}

var errNotFound = errors.New("not found")
