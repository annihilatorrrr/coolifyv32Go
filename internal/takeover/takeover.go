// Package takeover migrates running v3 workloads onto coolifygo's naming,
// labels, and network. Apps are recreated in place with the same image/env so
// the running process restart is brief and zero-config-drift. Databases get
// their data volumes safely copied to coolifygo's named volume before the
// fresh container is started.
package takeover

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/annihilatorrrr/coolifyv32Go/internal/discover"
)

const (
	// CoolifygoNetwork is the network coolifygo expects every workload on.
	CoolifygoNetwork = "coolifygo"
	// ManagedLabel is the label coolifygo's cleanup uses to scope prune ops.
	ManagedLabel = "coolifygo.managed"
)

// EnsureNetwork creates the coolifygo network if missing.
func EnsureNetwork(ctx context.Context, dc *client.Client) error {
	if _, err := dc.NetworkInspect(ctx, CoolifygoNetwork, network.InspectOptions{}); err == nil {
		return nil
	}
	_, err := dc.NetworkCreate(ctx, CoolifygoNetwork, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{ManagedLabel: "true"},
	})
	return err
}

// AppPlanInput is the minimal shape needed to take over an application.
type AppPlanInput struct {
	Workload *discover.V3Workload
	EnvVars  map[string]string // clean user env (decrypted v3 secrets); v3 platform vars filtered out
	NewID    string            // coolifygo applications.id as string
	Name     string
	Image    string // image to run (defaults to Workload.Image when blank)
	Port     int
	IsBot    bool // no service port; mirrors coolifygo's is_bot label + port-bind skip
	Start    bool // create-and-start when true; create-only when false (v3 had it stopped)
}

// TakeoverApp recreates the v3 app container as `coolifygo-<slug>-<id8>` with
// coolifygo's labels + network + the existing image/env. Returns the new
// container id. No-op when the v3 workload is nil (app not running on v3).
func TakeoverApp(ctx context.Context, dc *client.Client, in AppPlanInput) (string, error) {
	if in.Workload == nil {
		return "", nil
	}
	if len(in.NewID) < 8 {
		return "", fmt.Errorf("invalid new id %q", in.NewID)
	}
	newName := "coolifygo-" + slug(in.Name) + "-" + in.NewID[:8]
	imagee := in.Image
	if imagee == "" {
		imagee = in.Workload.Image
	}

	if err := stopAndRemove(ctx, dc, in.Workload.ContainerID); err != nil {
		return "", fmt.Errorf("stop v3 app container: %w", err)
	}

	labels := map[string]string{
		ManagedLabel:       "true",
		"coolifygo.app.id": in.NewID,
	}
	// Mirror deploy.BuildAppContainerConfig: bots carry the is_bot label and get
	// no host port binding (Port>0 && !IsBot is coolifygo's exact bind guard).
	if in.IsBot {
		labels["coolifygo.is_bot"] = "true"
	}
	hostCfg := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}
	exposed := nat.PortSet{}
	if in.Port > 0 && !in.IsBot {
		p := nat.Port(fmt.Sprintf("%d/tcp", in.Port))
		exposed[p] = struct{}{}
		hostCfg.PortBindings = nat.PortMap{p: []nat.PortBinding{{HostPort: fmt.Sprintf("%d", in.Port)}}}
	}

	env := make([]string, 0, len(in.EnvVars))
	for k, v := range in.EnvVars {
		env = append(env, k+"="+v)
	}

	resp, err := dc.ContainerCreate(ctx, &container.Config{
		Image:        imagee,
		Env:          env,
		Labels:       labels,
		ExposedPorts: exposed,
	}, hostCfg, &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			CoolifygoNetwork: {},
		},
	}, nil, newName)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", newName, err)
	}
	if !in.Start {
		return resp.ID, nil
	}
	if err = dc.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return resp.ID, fmt.Errorf("start %s: %w", newName, err)
	}
	return resp.ID, nil
}

// DBPlanInput is the minimal shape needed to take over a database.
type DBPlanInput struct {
	Workload     *discover.V3Workload
	NewID        string
	Type         string // "postgresql" | "redis"
	Version      string
	DBUser       string
	Password     string
	RootUser     string
	RootPassword string
	DefaultDB    string
	Slug         string // coolifygo slug; POSTGRES_DB fallback after DefaultDB
	Name         string // display name; final POSTGRES_DB fallback
	PublicPort   int
	InternalPort int
	IsPublic     bool
	AppendOnly   bool // Redis AOF; only consulted for Type == "redis"
	Start        bool // create-and-start when true; create-only when false (v3 had it stopped)
}

// TakeoverDB stops the v3 DB container, copies its data volume into a fresh
// coolifygo-named volume, then starts a `coolifygo-db-<id8>` container with
// the canonical image, env, healthcheck and labels.
func TakeoverDB(ctx context.Context, dc *client.Client, in DBPlanInput) (string, error) {
	if in.Workload == nil {
		return "", nil
	}
	if len(in.NewID) < 8 {
		return "", fmt.Errorf("invalid new id %q", in.NewID)
	}
	newName := "coolifygo-db-" + in.NewID[:8]
	newVol := newName

	srcVol := FindDataVolume(in.Workload, in.Type)
	if srcVol == "" {
		return "", fmt.Errorf("could not locate v3 data volume on container %s", in.Workload.ContainerName)
	}

	// Stop v3 DB first so SQLite/PG/Redis flush.
	if err := stopAndRemove(ctx, dc, in.Workload.ContainerID); err != nil {
		return "", fmt.Errorf("stop v3 db container: %w", err)
	}

	if err := copyVolume(ctx, dc, srcVol, newVol); err != nil {
		return "", fmt.Errorf("copy volume %s -> %s: %w", srcVol, newVol, err)
	}

	dataPath := dataPath(in.Type)
	img := dbImage(in.Type, in.Version)
	env, cmd := dbEnv(in)
	hc := dbHealthcheck(in.Type)

	labels := map[string]string{
		ManagedLabel:      "true",
		"coolifygo.db.id": in.NewID,
	}

	hostCfg := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: newVol,
			Target: dataPath,
		}},
	}
	exposed := nat.PortSet{}
	if in.IsPublic {
		hostPort := in.PublicPort
		if hostPort == 0 {
			hostPort = in.InternalPort
		}
		p := nat.Port(fmt.Sprintf("%d/tcp", in.InternalPort))
		exposed[p] = struct{}{}
		hostCfg.PortBindings = nat.PortMap{p: []nat.PortBinding{{HostPort: fmt.Sprintf("%d", hostPort)}}}
	}

	if err := pullImage(ctx, dc, img); err != nil {
		return "", fmt.Errorf("pull %s: %w", img, err)
	}

	resp, err := dc.ContainerCreate(ctx, &container.Config{
		Image:        img,
		Env:          env,
		Cmd:          cmd,
		Labels:       labels,
		ExposedPorts: exposed,
		Healthcheck:  hc,
	}, hostCfg, &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			CoolifygoNetwork: {},
		},
	}, nil, newName)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", newName, err)
	}
	if !in.Start {
		return resp.ID, nil
	}
	if err = dc.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return resp.ID, fmt.Errorf("start %s: %w", newName, err)
	}
	return resp.ID, nil
}

// WaitHealthy polls Docker until the container is running. Database health
// probe is best-effort; we cap at 2 min then give up (caller logs warning).
func WaitHealthy(ctx context.Context, dc *client.Client, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for container to become healthy")
		}
		info, err := dc.ContainerInspect(ctx, containerID)
		if err != nil {
			return err
		}
		// "exited" is the common terminal state when the entrypoint dies
		// (bad password, missing config, etc); Dead/OOMKilled are rarer
		// edge cases. Without "exited" here we'd poll until the timeout.
		st := info.State
		if st.Dead || st.OOMKilled || st.Status == "exited" {
			return fmt.Errorf("container exited (status=%s, exit=%d): %s",
				st.Status, st.ExitCode, st.Error)
		}
		if st.Running {
			if st.Health == nil {
				return nil // no healthcheck configured
			}
			if st.Health.Status == "healthy" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// stopAndRemove tolerates "not found" so a post-docker re-run after a partial
// takeover doesn't blow up on containers we already removed last time.
func stopAndRemove(ctx context.Context, dc *client.Client, id string) error {
	dc.ContainerStop(ctx, id, container.StopOptions{Timeout: new(30)})
	if err := dc.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// copyVolume copies the contents of srcVol into newVol via a one-shot alpine
// container that mounts both. The alpine image is tiny and ubiquitous; we
// pull it once if missing.
func copyVolume(ctx context.Context, dc *client.Client, srcVol, newVol string) error {
	if _, err := dc.VolumeInspect(ctx, newVol); err == nil {
		// New volume already exists from a previous failed run — wipe it so
		// we start clean rather than mixing old + new layouts.
		if err = dc.VolumeRemove(ctx, newVol, true); err != nil {
			return fmt.Errorf("clear stale new vol: %w", err)
		}
	}

	if err := pullImage(ctx, dc, "alpine:3"); err != nil {
		return fmt.Errorf("pull alpine: %w", err)
	}

	resp, err := dc.ContainerCreate(ctx, &container.Config{
		Image: "alpine:3",
		// cp -a preserves ownership, mode, and timestamps — sufficient on
		// its own; an additional chown step would just re-walk the tree.
		Cmd: []string{"sh", "-c", "cp -a /from/. /to/"},
	}, &container.HostConfig{
		AutoRemove: true,
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: srcVol, Target: "/from", ReadOnly: true},
			{Type: mount.TypeVolume, Source: newVol, Target: "/to"},
		},
	}, nil, nil, "coolfymigrater-volcopy-"+shortRand())
	if err != nil {
		return fmt.Errorf("create copy container: %w", err)
	}
	if err = dc.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start copy container: %w", err)
	}
	waitCh, errCh := dc.ContainerWait(ctx, resp.ID, container.WaitConditionRemoved)
	select {
	case result := <-waitCh:
		if result.StatusCode != 0 {
			return fmt.Errorf("volume copy exited with status %d", result.StatusCode)
		}
		return nil
	case err = <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func pullImage(ctx context.Context, dc *client.Client, ref string) error {
	if _, err := dc.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	rc, err := dc.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	io.Copy(io.Discard, rc) // drain so the pull completes
	return nil
}

// FindDataVolume picks the right Docker volume mount on a v3 DB container —
// PG keeps data at /var/lib/postgresql/data, redis at /data. Tolerates
// bind-mounts by skipping anything without a Source volume. Exported so the CLI
// can identify each DB's source volume for reclamation in teardown (takeover
// copies these into fresh coolifygo-db-<id8> volumes rather than moving them).
func FindDataVolume(w *discover.V3Workload, dbType string) string {
	target := dataPath(dbType)
	for _, m := range w.Volumes {
		if m.Type != mount.TypeVolume {
			continue
		}
		if m.Destination == target {
			return m.Name
		}
	}
	// Fallback: a single volume mount is the data volume.
	if len(w.Volumes) == 1 && w.Volumes[0].Type == mount.TypeVolume {
		return w.Volumes[0].Name
	}
	return ""
}

func dataPath(dbType string) string {
	switch dbType {
	case "postgresql":
		return "/var/lib/postgresql/data"
	case "redis":
		return "/data"
	}
	return "/data"
}

func dbImage(dbType, version string) string {
	if version == "" {
		version = "latest"
	}
	switch dbType {
	case "postgresql":
		return "postgres:" + version + "-alpine"
	case "redis":
		return "redis:" + version + "-alpine"
	}
	return dbType + ":" + version
}

func dbEnv(in DBPlanInput) (env []string, cmd []string) {
	// Mirror coolifygo dbEnv's default-database fallback exactly:
	// DefaultDatabase → Slug → Name. Only matters on a fresh-init (empty) volume;
	// a copied v3 volume already carries its databases, but we keep parity so a
	// coolifygo-driven recreate produces an identical container.
	defaultDB := firstNonEmpty(in.DefaultDB, in.Slug, in.Name)
	switch in.Type {
	case "postgresql":
		user := in.RootUser
		pw := in.RootPassword
		if user == "" {
			user = in.DBUser
			pw = in.Password
		}
		env = []string{
			"POSTGRES_USER=" + user,
			"POSTGRES_PASSWORD=" + pw,
			"POSTGRES_DB=" + defaultDB,
		}
		// PGUSER only when a user exists (coolifygo guards this) so we don't
		// pin libpq's default user to an empty string on a userless DB.
		if user != "" {
			env = append(env, "PGUSER="+user)
		}
	case "redis":
		// Mirror handler.dbCmd: emit CMD only when there's something the image
		// can't default — a password and/or AOF persistence. Order matters
		// (--requirepass before --appendonly) so the flags parse cleanly.
		if in.Password != "" {
			env = []string{"REDISCLI_AUTH=" + in.Password}
		}
		if in.Password != "" || in.AppendOnly {
			cmd = []string{"redis-server"}
			if in.Password != "" {
				cmd = append(cmd, "--requirepass", in.Password)
			}
			if in.AppendOnly {
				cmd = append(cmd, "--appendonly", "yes")
			}
		}
	}
	return env, cmd
}

func dbHealthcheck(dbType string) *container.HealthConfig {
	switch dbType {
	case "postgresql":
		// `-d postgres` pins the probe to the maintenance DB so libpq doesn't
		// fall back to PGDATABASE=PGUSER (which only exists if the user named
		// their default DB after the role). Matches coolifygo's dbHealthcheck.
		return &container.HealthConfig{
			Test:        []string{"CMD-SHELL", "pg_isready -h 127.0.0.1 -q -d postgres"},
			Interval:    5 * time.Second,
			Retries:     3,
			StartPeriod: 30 * time.Second,
		}
	case "redis":
		return &container.HealthConfig{
			Test:        []string{"CMD-SHELL", "redis-cli ping | grep -q PONG"},
			Interval:    3 * time.Second,
			Retries:     3,
			StartPeriod: 10 * time.Second,
		}
	}
	return nil
}

func slug(name string) string {
	s := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	if s == "" {
		return "app"
	}
	return s
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// shortRand returns 8 random hex chars for ephemeral container names.
func shortRand() string {
	var b [4]byte
	io.ReadFull(rand.Reader, b[:])
	return hex.EncodeToString(b[:])
}
