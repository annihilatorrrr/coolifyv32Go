// Command coolfymigrater migrates a running Coolify v3 install into coolifygo
// on the same host. It expects:
//   - the local Docker socket is available
//   - coolifygo is already provisioned (its postgres + redis are reachable
//     and at least one type=local server row exists)
//   - v3 manages only Dockerfile-built apps and postgresql / redis databases
//   - apps don't use persistent storage mounts
//
// Operator flow:
//
//	coolfymigrater --coolifygo-dsn=... --coolifygo-key=...
//	  → discover v3 → extract SQLite → plan → confirm → insert → takeover
//	    → verify → wipe v3
//
// All container ops happen on the local Docker daemon; SSH/remote-engine
// destinations from v3 are not migrated.
//
// # Phase-split execution
//
// Because install.sh upgrades the host's Docker engine between the data
// import and the container takeover, the binary supports running in two
// halves so the wrapper can sandwich the daemon restart cleanly:
//
//	coolfymigrater --phase=pre-docker  ...   # discover → freeze → extract → insert, then exit
//	(install.sh upgrades Docker; daemon comes back)
//	coolfymigrater --phase=post-docker ...   # reload plan → takeover → teardown
//
// State persists between invocations via a JSON file at --state-file
// (default /var/lib/coolfymigrater/state.json). --phase=all (default) keeps
// the original single-shot behaviour for operators running without the
// install.sh wrapper.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/annihilatorrrr/coolifyv32Go/internal/cgo"
	"github.com/annihilatorrrr/coolifyv32Go/internal/clilog"
	"github.com/annihilatorrrr/coolifyv32Go/internal/discover"
	"github.com/annihilatorrrr/coolifyv32Go/internal/mapper"
	"github.com/annihilatorrrr/coolifyv32Go/internal/takeover"
	"github.com/annihilatorrrr/coolifyv32Go/internal/teardown"
	v3pkg "github.com/annihilatorrrr/coolifyv32Go/internal/v3"
)

const (
	phaseAll        = "all"
	phasePreDocker  = "pre-docker"
	phasePostDocker = "post-docker"

	defaultStateFile = "/var/lib/coolfymigrater/state.json"
	stateVersion     = 1
)

type flags struct {
	coolifygoDSN string
	coolifygoKey string
	v3SecretKey  string
	v3SQLite     string
	phase        string
	stateFile    string
	dryRun       bool
	yes          bool
	noTeardown   bool
	oldfix       bool
}

// state is what `pre-docker` writes to disk and `post-docker` reads back.
// The full plan carries every NewID that the insert phase allocated plus the
// v3 workload metadata takeover needs — so the Docker daemon restart in
// between can mangle live container state freely without losing context.
type state struct {
	Plan    *mapper.Plan `json:"plan"`
	Version int          `json:"version"`
}

// errDryRun unwinds runPreDocker cleanly without the caller treating it as a
// real failure. We can't return a nil plan + nil error there because nil-plan
// would crash the caller.
var errDryRun = errors.New("dry-run complete")

func main() {
	f := parseFlags()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, f); err != nil {
		if errors.Is(err, errDryRun) {
			return
		}
		clilog.Fail("%s", err.Error())
		os.Exit(1)
	}
	clilog.OK("migration complete")
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.coolifygoDSN, "coolifygo-dsn", env("DATABASE_URL", ""),
		"coolifygo postgres DSN (defaults to $DATABASE_URL)")
	flag.StringVar(&f.coolifygoKey, "coolifygo-key", env("DATA_ENCRYPTION_KEY", ""),
		"coolifygo DATA_ENCRYPTION_KEY (base64, 32 raw bytes)")
	flag.StringVar(&f.v3SecretKey, "v3-secret-key", "",
		"v3 COOLIFY_SECRET_KEY override (auto-detected from coolify container env when blank)")
	flag.StringVar(&f.v3SQLite, "v3-sqlite", "",
		"path to v3 prod.db on host (auto-extracted from coolify container when blank)")
	flag.StringVar(&f.phase, "phase", phaseAll,
		"execution phase: 'all' (default, single-shot), 'pre-docker' (discover→insert), 'post-docker' (takeover→teardown). Used by install.sh to bracket a Docker engine upgrade.")
	flag.StringVar(&f.stateFile, "state-file", defaultStateFile,
		"path where pre-docker writes / post-docker reads the migration plan JSON")
	flag.BoolVar(&f.dryRun, "dry-run", false, "print the migration plan and exit without changing anything")
	flag.BoolVar(&f.yes, "yes", false, "skip interactive confirmation prompts")
	flag.BoolVar(&f.noTeardown, "no-teardown", false, "skip the v3 wipe phase")
	flag.BoolVar(&f.oldfix, "oldfix", false,
		"repair a broken/interrupted takeover: adopt leftover running containers into coolifygo's naming, network, and DB rows without rebuilding images or touching v3 data")
	flag.Parse()
	return f
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(ctx context.Context, f flags) error {
	if f.coolifygoDSN == "" {
		return fmt.Errorf("--coolifygo-dsn (or $DATABASE_URL) is required")
	}
	if f.coolifygoKey == "" {
		return fmt.Errorf("--coolifygo-key (or $DATA_ENCRYPTION_KEY) is required")
	}

	// --oldfix is a standalone repair mode: it reads only coolifygo's Postgres
	// and the live Docker containers, so it bypasses the v3-dependent phase
	// pipeline entirely.
	if f.oldfix {
		return runOldFix(ctx, f)
	}

	switch f.phase {
	case phaseAll, phasePreDocker, phasePostDocker:
	default:
		return fmt.Errorf("--phase %q invalid (want all|pre-docker|post-docker)", f.phase)
	}

	if f.phase == phasePostDocker {
		return runPostDocker(ctx, f)
	}

	plan, err := runPreDocker(ctx, f)
	if err != nil {
		return err
	}
	if f.phase == phasePreDocker {
		if err = saveState(f.stateFile, plan); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
		clilog.OK("plan saved to %s — invoke --phase=post-docker after Docker upgrade", f.stateFile)
		return nil
	}
	return runTakeoverAndTeardown(ctx, f, plan)
}

// runPreDocker performs every step that depends on the v3 SQLite, runs the
// transactional insert into coolifygo's Postgres, and returns the populated
// plan (NewIDs filled). After this returns, the Docker engine may be torn
// down and replaced — v3's data is durably persisted, and takeover only
// needs the named volumes (which survive a daemon restart).
func runPreDocker(ctx context.Context, f flags) (*mapper.Plan, error) {
	clilog.Phase(1, 7, "discover v3 stack")
	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	defer dc.Close()

	stack, err := discover.Inspect(ctx, dc)
	if err != nil {
		return nil, err
	}
	clilog.OK("found `coolify` container %s", short(stack.CoolifyContainerID))
	clilog.OK("found %d v3-managed workload container(s) (label coolify.managed=true)", len(stack.WorkloadContainers))
	if f.v3SecretKey != "" {
		stack.SecretKey = f.v3SecretKey
	}

	clilog.Phase(2, 7, "connect coolifygo postgres")
	target, err := cgo.Open(ctx, f.coolifygoDSN, f.coolifygoKey)
	if err != nil {
		return nil, err
	}
	defer target.Close()
	localServer, err := target.FindLocalServer(ctx)
	if err != nil {
		return nil, err
	}
	clilog.OK("local server row: %s", localServer)

	clilog.Phase(3, 7, "freeze v3 management containers")
	if stack.CoolifyContainerID != "" {
		if err = stopContainer(ctx, dc, stack.CoolifyContainerID); err != nil {
			clilog.Warn("stop coolify: %s", err)
		} else {
			clilog.OK("stopped coolify")
		}
	}
	if stack.FluentBitID != "" {
		stopContainer(ctx, dc, stack.FluentBitID)
		clilog.OK("stopped coolify-fluentbit")
	}

	clilog.Phase(4, 7, "extract + read v3 sqlite")
	sqlitePath := f.v3SQLite
	if sqlitePath == "" {
		// docker cp works on stopped containers — the read-only filesystem
		// snapshot is mounted into the daemon. v3 was stopped above so the
		// SQLite file is fsync'd + consistent.
		extracted, eerr := discover.ExtractSQLite(ctx, dc, stack.CoolifyContainerID)
		if eerr != nil {
			return nil, fmt.Errorf("extract sqlite: %w", eerr)
		}
		sqlitePath = extracted
		defer os.RemoveAll(filepath.Dir(sqlitePath))
	}

	v3c, err := v3pkg.Open(sqlitePath, stack.SecretKey)
	if err != nil {
		return nil, err
	}
	defer v3c.Close()

	apps, err := v3c.Applications()
	if err != nil {
		return nil, err
	}
	dbs, err := v3c.Databases()
	if err != nil {
		return nil, err
	}
	sources, err := v3c.GitSources()
	if err != nil {
		return nil, err
	}
	ghApps, err := v3c.GitHubApps()
	if err != nil {
		return nil, err
	}
	clilog.OK("read v3: %d apps, %d dbs, %d git sources, %d github apps",
		len(apps), len(dbs), len(sources), len(ghApps))

	clilog.Phase(5, 7, "build migration plan")
	plan, err := mapper.BuildPlan(localServer, apps, dbs, sources, ghApps, stack.WorkloadContainers)
	if err != nil {
		return nil, err
	}

	existingPorts, err := target.UsedPorts(ctx, localServer)
	if err != nil {
		return nil, fmt.Errorf("check existing ports: %w", err)
	}
	if err = plan.ValidatePortsAgainst(existingPorts); err != nil {
		return nil, err
	}

	printPlan(plan)

	if f.dryRun {
		clilog.Info("--dry-run set; exiting before any change")
		return nil, errDryRun
	}

	if !clilog.Confirm("Proceed with migration?", f.yes) {
		return nil, fmt.Errorf("aborted by user")
	}

	if err = insertAll(ctx, target, plan); err != nil {
		return nil, fmt.Errorf("insert phase: %w", err)
	}
	clilog.OK("inserted %d git source(s), %d app(s), %d db(s)",
		len(plan.GitSources), len(plan.Apps), len(plan.Databases))
	return plan, nil
}

// runPostDocker resumes after Docker has been upgraded by install.sh. It
// re-opens the docker client (the daemon may have a new version + reloaded
// socket), reloads the plan written by --phase=pre-docker, then performs
// the container takeover and the optional v3 teardown.
func runPostDocker(ctx context.Context, f flags) error {
	plan, err := loadState(f.stateFile)
	if err != nil {
		return fmt.Errorf("load state %s: %w", f.stateFile, err)
	}
	clilog.OK("resumed from %s — %d apps + %d dbs queued for takeover",
		f.stateFile, len(plan.Apps), len(plan.Databases))
	return runTakeoverAndTeardown(ctx, f, plan)
}

// runTakeoverAndTeardown is the second half of the run, shared by --phase=all
// (which arrives here with a fresh plan) and --phase=post-docker (which
// rehydrated the plan from disk).
func runTakeoverAndTeardown(ctx context.Context, f flags, plan *mapper.Plan) error {
	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dc.Close()

	// Reopen coolifygo's Postgres so takeover can persist the real container ids
	// back onto the rows the insert phase created. The DSN + key are already
	// required for every phase (see run), so post-docker resumes need no extra
	// flags. Best-effort: a writeback failure is logged, not fatal — the
	// containers are already live and the reconciler heals stale ids on restart.
	target, err := cgo.Open(ctx, f.coolifygoDSN, f.coolifygoKey)
	if err != nil {
		return err
	}
	defer target.Close()

	clilog.Phase(6, 7, "take over running containers")
	if err = takeover.EnsureNetwork(ctx, dc); err != nil {
		return fmt.Errorf("ensure coolifygo network: %w", err)
	}
	reclaimVols, err := runTakeover(ctx, dc, target, plan)
	if err != nil {
		return fmt.Errorf("takeover phase: %w", err)
	}

	if f.noTeardown {
		clilog.Phase(7, 7, "teardown (skipped — --no-teardown)")
		return nil
	}

	// Completeness gate: refuse to wipe v3 while any discovered container went
	// unmatched. Wiping here is what turned an earlier incomplete takeover into
	// an unrecoverable state (v3 SQLite + volumes gone, workloads stranded).
	// Overrides --yes on purpose — never destroy v3 when the picture is partial.
	if len(plan.OrphanWorkloads) > 0 {
		clilog.Phase(7, 7, "teardown (refused — incomplete migration)")
		clilog.Warn("%d v3 container(s) were discovered but not migrated:", len(plan.OrphanWorkloads))
		for _, o := range plan.OrphanWorkloads {
			clilog.Warn("  - %s (%s)", o.Name, short(o.ContainerID))
		}
		clilog.Warn("v3 is left intact so nothing is stranded. Investigate the unmatched containers, fix the mapping, and re-run — or wipe v3 by hand once you've confirmed they're safe to drop.")
		return nil
	}

	clilog.Phase(7, 7, "wipe v3 install")
	if !clilog.Confirm("Wipe v3 containers, volumes (incl. copied-from database volumes), images, network, host paths?", f.yes) {
		clilog.Warn("teardown skipped by user")
		return nil
	}
	if err = teardown.Wipe(ctx, dc, os.Stdout, reclaimVols); err != nil {
		clilog.Warn("%s", err)
	}

	// State file was a hand-off artifact between phases. Once we've taken
	// over + torn down, nothing should ever read it again, and leaving it
	// around invites a confused re-run. Drop the now-empty state dir too so a
	// completed migration leaves nothing on disk. os.Remove only deletes an
	// empty dir, so a shared or custom --state-file location that still holds
	// other files is left untouched.
	if f.phase == phasePostDocker && f.stateFile != "" {
		os.Remove(f.stateFile)
		os.Remove(filepath.Dir(f.stateFile))
	}
	return nil
}

// runOldFix repairs a host left broken by an interrupted takeover: coolifygo
// holds the application rows but the workload containers are still running
// under their old v3 names (never renamed), so coolifygo shows them
// stopped / container-less. It reads ONLY coolifygo's Postgres and the live
// Docker daemon — no v3 SQLite, no state file, no image rebuild — and adopts
// each leftover container by renaming it to coolifygo's stable name, attaching
// it to the coolifygo network, and writing the live container id, image, host
// port, and running status back onto the row. A safe no-op when nothing is
// broken; re-runnable.
func runOldFix(ctx context.Context, f flags) error {
	clilog.Phase(1, 3, "connect docker + coolifygo")
	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dc.Close()

	target, err := cgo.Open(ctx, f.coolifygoDSN, f.coolifygoKey)
	if err != nil {
		return err
	}
	defer target.Close()

	server, err := target.FindLocalServer(ctx)
	if err != nil {
		return err
	}
	clilog.OK("local server row: %s", server)

	apps, err := target.ListApplications(ctx, server)
	if err != nil {
		return fmt.Errorf("list coolifygo applications: %w", err)
	}
	clilog.OK("coolifygo has %d application row(s)", len(apps))

	// A single running-container snapshot drives both the "already adopted?"
	// check and the leftover-candidate list — no per-container inspect.
	running, err := dc.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return fmt.Errorf("list running containers: %w", err)
	}
	type live struct {
		id   string
		port int
	}
	byName := make(map[string]live, len(running))
	var candidates []mapper.AdoptCandidate
	for i := range running {
		c := running[i]
		name := stripName(c.Names)
		port := lowestPublicPort(c.Ports)
		byName[name] = live{id: c.ID, port: port}
		// A candidate is a leftover workload container: anything not already
		// coolifygo-managed. We deliberately do NOT require v3's
		// `coolify.managed` label — the matching failure that stranded these
		// containers may itself be a labelling quirk, so we stay lenient and
		// lean on the bijective name match + interactive confirmation for
		// safety rather than a filter that might exclude the very containers we
		// need to fix.
		if strings.HasPrefix(name, "coolifygo") || c.Labels["coolifygo.managed"] == "true" {
			continue
		}
		candidates = append(candidates, mapper.AdoptCandidate{
			ID: c.ID, Name: name, Image: c.Image, HostPort: port,
		})
	}

	clilog.Phase(2, 3, "match orphaned rows to live containers")
	var broken []cgo.AppInfo
	refreshed := 0
	for _, a := range apps {
		expected := mapper.AppContainerName(a.ID, a.Name)
		lv, ok := byName[expected]
		if !ok {
			broken = append(broken, a)
			continue
		}
		// Already running under the coolifygo name. Refresh the row only if it
		// has drifted (empty/stale container_id or non-running status) so
		// Stop/Logs/Stats target the live container.
		if a.ContainerID == lv.id && a.Status == "running" {
			continue
		}
		if uerr := target.AdoptApplication(ctx, a.ID, lv.id, a.ImageName, lv.port); uerr != nil {
			clilog.Warn("%s: refresh row: %s", a.Name, uerr)
			continue
		}
		clilog.OK("%s already running as %s — row refreshed", a.Name, expected)
		refreshed++
	}

	targets, unmatched := mapper.MatchAdoptable(broken, candidates)
	for _, u := range unmatched {
		clilog.Warn("%s", u)
	}
	if len(targets) == 0 {
		switch {
		case refreshed > 0:
			clilog.OK("refreshed %d already-running app row(s); nothing else to adopt", refreshed)
		default:
			clilog.OK("nothing to adopt — every application row already maps to a running container")
		}
		return nil
	}

	clilog.Info("proposed adoptions:")
	for _, t := range targets {
		portNote := ""
		if t.HostPort > 0 {
			portNote = fmt.Sprintf(", host port %d", t.HostPort)
		}
		clilog.Info("  - %s (%s) → %s%s", t.AppName, short(t.LiveID), t.NewName, portNote)
	}

	clilog.Phase(3, 3, "adopt containers")
	if !clilog.Confirm("Rename these live containers into coolifygo and update their rows?", f.yes) {
		clilog.Warn("aborted by user")
		return nil
	}

	adopted := 0
	for _, t := range targets {
		if err = takeover.AdoptApp(ctx, dc, t.LiveID, t.NewName); err != nil {
			clilog.Warn("%s: %s", t.AppName, err)
			continue
		}
		// Network attach is best-effort: coolifygo manages by name + id without
		// it. A failure here must not block the row writeback.
		if aerr := takeover.AttachToCoolifygoNetwork(ctx, dc, t.LiveID); aerr != nil {
			clilog.Warn("%s: network attach: %s", t.AppName, aerr)
		}
		if err = target.AdoptApplication(ctx, t.AppID, t.LiveID, t.LiveImage, t.HostPort); err != nil {
			clilog.Warn("%s: renamed but row writeback failed: %s", t.AppName, err)
			continue
		}
		clilog.OK("%s adopted as %s", t.AppName, t.NewName)
		adopted++
	}
	if refreshed > 0 {
		clilog.OK("adopted %d of %d container(s); also refreshed %d already-running row(s)", adopted, len(targets), refreshed)
	} else {
		clilog.OK("adopted %d of %d container(s)", adopted, len(targets))
	}
	return nil
}

// stripName returns the first container name without Docker's leading slash.
func stripName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// lowestPublicPort returns the lowest published host port across a container's
// port list (0 if none). Mirrors mapper.firstHostPort's deterministic choice so
// an adopted row carries the same host-port semantics coolifygo's Port uses.
func lowestPublicPort(ports []container.Port) int {
	lowest := 0
	for _, p := range ports {
		if p.PublicPort == 0 {
			continue
		}
		hp := int(p.PublicPort)
		if lowest == 0 || hp < lowest {
			lowest = hp
		}
	}
	return lowest
}

func saveState(path string, plan *mapper.Plan) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(state{Version: stateVersion, Plan: plan}, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: rename is atomic on the same filesystem, so a crash
	// mid-flush can't leave us with half a plan.
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadState(path string) (*mapper.Plan, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s state
	if err = json.Unmarshal(buf, &s); err != nil {
		return nil, err
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("state file version %d, want %d — rerun --phase=pre-docker", s.Version, stateVersion)
	}
	if s.Plan == nil {
		return nil, fmt.Errorf("state file has no plan")
	}
	return s.Plan, nil
}

// insertAll runs the entire DB-write phase inside a single transaction. If
// anything fails after partial inserts, the rollback restores coolifygo's DB
// to its pre-migration state. Container takeover runs only after this commits.
func insertAll(ctx context.Context, target *cgo.Client, plan *mapper.Plan) error {
	tx, err := target.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Git sources first — apps reference them by ID. One-shot: drop the
	// coolifygo DB before re-running (greenfield rule). Same-name apps and
	// DBs are legal in both v3 and coolifygo so we don't dedupe on name.
	for i := range plan.GitSources {
		gs := plan.GitSources[i]
		newID, err := target.InsertGitSource(ctx, tx, gs.Row)
		if err != nil {
			return err
		}
		plan.SetGitSourceID(gs.V3SourceID, newID)
	}

	for i := range plan.Apps {
		ap := &plan.Apps[i]
		newID, err := target.InsertApplication(ctx, tx, ap.Row)
		if err != nil {
			return err
		}
		ap.NewID = newID
	}

	for i := range plan.Databases {
		dp := &plan.Databases[i]
		newID, err := target.InsertDatabase(ctx, tx, dp.Row)
		if err != nil {
			return err
		}
		dp.NewID = newID
	}

	return tx.Commit(ctx)
}

// runTakeover recreates every workload container and returns the set of v3
// database source volumes takeover copied from, so teardown can reclaim them.
func runTakeover(ctx context.Context, dc *client.Client, target *cgo.Client, plan *mapper.Plan) ([]string, error) {
	var reclaimVols []string
	for i := range plan.Apps {
		ap := plan.Apps[i]
		if ap.NewID == uuid.Nil {
			continue
		}
		// A nil workload means no v3 container was matched to this row. Never
		// silent: an app that should have a container but didn't match is the
		// bug that strands workloads, and it also surfaces as an orphan that
		// blocks teardown.
		if ap.Workload == nil {
			clilog.Warn("%s: no v3 container matched — left as status=%s, not taken over", ap.Row.Name, ap.Row.Status)
			continue
		}
		start := ap.Workload.Running
		verb := "recreate (start)"
		if !start {
			verb = "recreate (stopped — leave stopped)"
		}
		clilog.Info("%s %s → coolifygo-…-%s",
			verb, ap.Workload.ContainerName, short(ap.NewID.String()))
		newCID, err := takeover.TakeoverApp(ctx, dc, takeover.AppPlanInput{
			NewID:    ap.NewID.String(),
			Name:     ap.Row.Name,
			Workload: ap.Workload,
			EnvVars:  ap.Row.EnvVars,
			Image:    ap.Row.ImageName,
			Port:     ap.Row.Port,
			IsBot:    ap.Row.IsBot,
			Start:    start,
		})
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", ap.Row.Name, err)
		}
		status := "running"
		if !start {
			status = "stopped"
		}
		if uerr := target.UpdateAppContainer(ctx, ap.NewID, newCID, status); uerr != nil {
			clilog.Warn("%s: persist container id: %s", ap.Row.Name, uerr)
		}
		if !start {
			clilog.OK("%s created (status=stopped)", ap.Row.Name)
			continue
		}
		if err = takeover.WaitHealthy(ctx, dc, newCID, 60*time.Second); err != nil {
			clilog.Warn("%s: %s", ap.Row.Name, err)
		} else {
			clilog.OK("%s running", ap.Row.Name)
		}
	}

	for i := range plan.Databases {
		dp := plan.Databases[i]
		if dp.NewID == uuid.Nil {
			continue
		}
		if dp.Workload == nil {
			clilog.Warn("%s: no v3 container matched — left as status=%s, not taken over", dp.Row.Name, dp.Row.Status)
			continue
		}
		start := dp.Workload.Running
		verb := "recreate (start)"
		if !start {
			verb = "recreate (stopped — leave stopped)"
		}
		clilog.Info("%s %s → coolifygo-db-%s (volume copy)",
			verb, dp.Workload.ContainerName, short(dp.NewID.String()))
		newCID, err := takeover.TakeoverDB(ctx, dc, takeover.DBPlanInput{
			NewID:        dp.NewID.String(),
			Type:         dp.Row.Type,
			Version:      dp.Row.Version,
			DBUser:       dp.Row.DBUser,
			Password:     dp.Row.Password,
			RootUser:     dp.Row.RootUser,
			RootPassword: dp.Row.RootPassword,
			DefaultDB:    dp.Row.DefaultDatabase,
			Slug:         dp.Row.Slug,
			Name:         dp.Row.Name,
			PublicPort:   dp.Row.PublicPort,
			InternalPort: dp.Row.InternalPort,
			IsPublic:     dp.Row.IsPublic,
			AppendOnly:   dp.Row.AppendOnly,
			Workload:     dp.Workload,
			Start:        start,
		})
		if err != nil {
			return nil, fmt.Errorf("db %s: %w", dp.Row.Name, err)
		}
		// Takeover copied this DB's v3 data volume into a fresh coolifygo volume;
		// record the source so teardown can reclaim it. Recorded for stopped DBs
		// too — the copy happened regardless of whether we started the container.
		if srcVol := takeover.FindDataVolume(dp.Workload, dp.Row.Type); srcVol != "" {
			reclaimVols = append(reclaimVols, srcVol)
		}
		status := "running"
		if !start {
			status = "stopped"
		}
		if uerr := target.UpdateDBContainer(ctx, dp.NewID, newCID, status); uerr != nil {
			clilog.Warn("%s: persist container id: %s", dp.Row.Name, uerr)
		}
		if !start {
			clilog.OK("%s created (status=stopped)", dp.Row.Name)
			continue
		}
		if err = takeover.WaitHealthy(ctx, dc, newCID, 2*time.Minute); err != nil {
			clilog.Warn("%s: %s", dp.Row.Name, err)
		} else {
			clilog.OK("%s healthy", dp.Row.Name)
		}
	}
	return reclaimVols, nil
}

func printPlan(plan *mapper.Plan) {
	clilog.Info("git sources: %d", len(plan.GitSources))
	for _, g := range plan.GitSources {
		clilog.Info("  - %s (app_id=%d)", g.Row.Name, g.Row.AppID)
	}
	clilog.Info("applications: %d", len(plan.Apps))
	for _, a := range plan.Apps {
		status := "stopped"
		if a.Workload != nil && a.Workload.Running {
			status = "running"
		}
		clilog.Info("  - %s (port=%d, %s)", a.Row.Name, a.Row.Port, status)
	}
	clilog.Info("databases: %d", len(plan.Databases))
	for _, d := range plan.Databases {
		status := "stopped"
		if d.Workload != nil && d.Workload.Running {
			status = "running"
		}
		clilog.Info("  - %s (%s, %s)", d.Row.Name, d.Row.Type, status)
	}
	if len(plan.OrphanWorkloads) > 0 {
		clilog.Warn("unmatched v3 containers (claimed by no app/db): %d", len(plan.OrphanWorkloads))
		for _, o := range plan.OrphanWorkloads {
			clilog.Warn("  - %s (%s)", o.Name, short(o.ContainerID))
		}
		clilog.Warn("teardown will be refused while these exist — they would otherwise be destroyed with v3")
	}
}

func stopContainer(ctx context.Context, dc *client.Client, id string) error {
	return dc.ContainerStop(ctx, id, container.StopOptions{Timeout: new(30)})
}

func short(id string) string {
	if len(id) < 12 {
		return id
	}
	return id[:12]
}
