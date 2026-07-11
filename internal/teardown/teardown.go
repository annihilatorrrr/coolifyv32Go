// Package teardown removes a v3 install from the local host once migration
// has been verified. It stops and removes v3's own stack containers, drops
// the volumes that held v3's runtime (SQLite, logs, Let's-Encrypt certs),
// prunes the coollabsio images, and tears down the coolify-infra network.
//
// User app/DB volumes are NEVER touched here — those have already been copied
// (databases) or relabeled (apps) by the takeover phase, and ownership of
// any leftover bind mounts belongs to the user.
package teardown

import (
	"context"
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

// HostPaths lists v3 install artifacts on the host filesystem. Removed last,
// after the docker objects, so a partial failure earlier leaves the host
// state intact and re-runnable.
var HostPaths = []string{
	"/data/coolify",
	"/var/lib/coolify",
	"/etc/cron.d/coolify-default",
	"/usr/local/bin/coolify",
}

// Wipe performs the full v3 teardown. Caller has already confirmed (no
// prompting here). sourceVols carries the original v3 database volumes that
// takeover copied into fresh coolifygo-db-<id8> volumes — see reclaimSourceVolumes.
// Returns a multi-error of non-fatal issues that didn't block the overall wipe.
func Wipe(ctx context.Context, dc *client.Client, w io.Writer, sourceVols []string) error {
	var nonFatal []string

	for _, name := range V3Containers {
		if id, err := containerByName(ctx, dc, name); err == nil && id != "" {
			fmt.Fprintf(w, "  stop+remove %s\n", name)
			dc.ContainerStop(ctx, id, container.StopOptions{Timeout: new(30)})
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

	nonFatal = append(nonFatal, reclaimSourceVolumes(ctx, dc, w, sourceVols)...)

	if err := removeImagesByPrefix(ctx, dc, w, "coollabsio/coolify"); err != nil {
		nonFatal = append(nonFatal, err.Error())
	}
	if err := removeImagesByPrefix(ctx, dc, w, "ghcr.io/coollabsio/coolify"); err != nil {
		nonFatal = append(nonFatal, err.Error())
	}
	if err := removeImagesByPrefix(ctx, dc, w, "ghcr.io/coollabsio/fluent-bit"); err != nil {
		nonFatal = append(nonFatal, err.Error())
	}

	// alpine:3 is the one image the migrater itself pulls (the volume-copy
	// helper base). Drop it too so nothing we introduced lingers — but only when
	// unused: Force=false makes Docker refuse if any container still references
	// it, so a user workload built on alpine:3 is never yanked out from under
	// them. It re-pulls on demand if a later run needs it again.
	if err := removeImageIfUnused(ctx, dc, w, "alpine:3"); err != nil {
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

// reclaimSourceVolumes removes the original v3 database volumes that takeover
// copied FROM (it copies rather than moves, so the source lingers). Every guard
// here is deliberate so we can never destroy the wrong data:
//   - skip empty names and anything coolifygo-owned (never touch a copy target
//     or any coolifygo-managed volume),
//   - dedupe so the same volume isn't attempted twice,
//   - inspect-before-remove, so an already-gone volume is a silent no-op
//     (keeps the post-docker re-run model intact),
//   - force=false so Docker refuses to remove a volume still mounted by any
//     container — the v3 DB container is already gone by this point, but if
//     anything still holds the volume we leave it alone and report it.
func reclaimSourceVolumes(ctx context.Context, dc *client.Client, w io.Writer, sourceVols []string) []string {
	var nonFatal []string
	seen := make(map[string]bool, len(sourceVols))
	for _, vol := range sourceVols {
		if vol == "" || strings.HasPrefix(vol, "coolifygo-") || seen[vol] {
			continue
		}
		seen[vol] = true
		if _, err := dc.VolumeInspect(ctx, vol); err != nil {
			continue // already removed, or never existed
		}
		fmt.Fprintf(w, "  remove v3 source volume %s\n", vol)
		if err := dc.VolumeRemove(ctx, vol, false); err != nil {
			nonFatal = append(nonFatal, fmt.Sprintf("remove source volume %s (still in use?): %s", vol, err))
		}
	}
	return nonFatal
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

// removeImageIfUnused drops a single image by ref with Force=false, so Docker
// refuses (and we report non-fatally) when any container still references it.
// Absent image is a silent no-op. Used for alpine:3, the migrater's copy helper.
func removeImageIfUnused(ctx context.Context, dc *client.Client, w io.Writer, ref string) error {
	if _, err := dc.ImageInspect(ctx, ref); err != nil {
		return nil // not present — nothing to remove
	}
	fmt.Fprintf(w, "  remove image %s\n", ref)
	if _, err := dc.ImageRemove(ctx, ref, image.RemoveOptions{Force: false, PruneChildren: true}); err != nil {
		return fmt.Errorf("remove image %s (in use — kept): %w", ref, err)
	}
	return nil
}

func removeNetwork(ctx context.Context, dc *client.Client, w io.Writer, name string) error {
	if err := dc.NetworkRemove(ctx, name); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil
		}
		return fmt.Errorf("remove network %s: %w", name, err)
	}
	fmt.Fprintf(w, "  remove network %s\n", name)
	return nil
}
