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
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"coolfymigrater/internal/cgo"
	"coolfymigrater/internal/clilog"
	"coolfymigrater/internal/discover"
	"coolfymigrater/internal/mapper"
	"coolfymigrater/internal/takeover"
	"coolfymigrater/internal/teardown"
	v3pkg "coolfymigrater/internal/v3"
)

type flags struct {
	coolifygoDSN string
	coolifygoKey string
	v3SecretKey  string
	v3SQLite     string
	dryRun       bool
	yes          bool
	noTeardown   bool
}

func main() {
	f := parseFlags()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, f); err != nil {
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
	flag.BoolVar(&f.dryRun, "dry-run", false, "print the migration plan and exit without changing anything")
	flag.BoolVar(&f.yes, "yes", false, "skip interactive confirmation prompts")
	flag.BoolVar(&f.noTeardown, "no-teardown", false, "skip the v3 wipe phase")
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

	clilog.Phase(1, 7, "discover v3 stack")
	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dc.Close()

	stack, err := discover.Inspect(ctx, dc)
	if err != nil {
		return err
	}
	clilog.OK("found `coolify` container %s", short(stack.CoolifyContainerID))
	clilog.OK("found %d workload container(s) on coolify-infra", len(stack.WorkloadContainers))
	if f.v3SecretKey != "" {
		stack.SecretKey = f.v3SecretKey
	}

	clilog.Phase(2, 7, "connect coolifygo postgres")
	target, err := cgo.Open(ctx, f.coolifygoDSN, f.coolifygoKey)
	if err != nil {
		return err
	}
	defer target.Close()
	localServer, err := target.FindLocalServer(ctx)
	if err != nil {
		return err
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
			return fmt.Errorf("extract sqlite: %w", eerr)
		}
		sqlitePath = extracted
		defer os.RemoveAll(filepath.Dir(sqlitePath))
	}

	v3c, err := v3pkg.Open(sqlitePath, stack.SecretKey)
	if err != nil {
		return err
	}
	defer v3c.Close()

	apps, err := v3c.Applications()
	if err != nil {
		return err
	}
	dbs, err := v3c.Databases()
	if err != nil {
		return err
	}
	sources, err := v3c.GitSources()
	if err != nil {
		return err
	}
	ghApps, err := v3c.GitHubApps()
	if err != nil {
		return err
	}
	clilog.OK("read v3: %d apps, %d dbs, %d git sources, %d github apps",
		len(apps), len(dbs), len(sources), len(ghApps))

	clilog.Phase(5, 7, "build migration plan")
	plan, err := mapper.BuildPlan(localServer, apps, dbs, sources, ghApps, stack.WorkloadContainers)
	if err != nil {
		return err
	}
	printPlan(plan)

	if f.dryRun {
		clilog.Info("--dry-run set; exiting before any change")
		return nil
	}

	if !clilog.Confirm("Proceed with migration?", f.yes) {
		return fmt.Errorf("aborted by user")
	}

	if err = insertAll(ctx, target, plan); err != nil {
		return fmt.Errorf("insert phase: %w", err)
	}
	clilog.OK("inserted %d git source(s), %d app(s), %d db(s)",
		len(plan.GitSources), len(plan.Apps), len(plan.Databases))

	clilog.Phase(6, 7, "take over running containers")
	if err = takeover.EnsureNetwork(ctx, dc); err != nil {
		return fmt.Errorf("ensure coolifygo network: %w", err)
	}
	if err = runTakeover(ctx, dc, plan); err != nil {
		return fmt.Errorf("takeover phase: %w", err)
	}

	if f.noTeardown {
		clilog.Phase(7, 7, "teardown (skipped — --no-teardown)")
		return nil
	}

	clilog.Phase(7, 7, "wipe v3 install")
	if !clilog.Confirm("Wipe v3 containers, volumes, images, network, host paths?", f.yes) {
		clilog.Warn("teardown skipped by user")
		return nil
	}
	if err = teardown.Wipe(ctx, dc, os.Stdout); err != nil {
		clilog.Warn("%s", err)
	}
	return nil
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

func runTakeover(ctx context.Context, dc *client.Client, plan *mapper.Plan) error {
	for i := range plan.Apps {
		ap := plan.Apps[i]
		if ap.NewID == uuid.Nil || ap.Workload == nil {
			continue
		}
		start := ap.Workload.Running
		verb := "recreate (start)"
		if !start {
			verb = "recreate (stopped — leave stopped)"
		}
		clilog.Info("%s %s → coolifygo-…-%s",
			verb, ap.Workload.ContainerName, short(ap.NewID.String()))
		newID, err := takeover.TakeoverApp(ctx, dc, takeover.AppPlanInput{
			NewID:    ap.NewID.String(),
			Name:     ap.Row.Name,
			Workload: ap.Workload,
			EnvVars:  ap.Row.EnvVars,
			Image:    ap.Row.ImageName,
			Port:     ap.Row.Port,
			Start:    start,
		})
		if err != nil {
			return fmt.Errorf("app %s: %w", ap.Row.Name, err)
		}
		if !start {
			clilog.OK("%s created (status=stopped)", ap.Row.Name)
			continue
		}
		if err = takeover.WaitHealthy(ctx, dc, newID, 60*time.Second); err != nil {
			clilog.Warn("%s: %s", ap.Row.Name, err)
		} else {
			clilog.OK("%s running", ap.Row.Name)
		}
	}

	for i := range plan.Databases {
		dp := plan.Databases[i]
		if dp.NewID == uuid.Nil || dp.Workload == nil {
			continue
		}
		start := dp.Workload.Running
		verb := "recreate (start)"
		if !start {
			verb = "recreate (stopped — leave stopped)"
		}
		clilog.Info("%s %s → coolifygo-db-%s (volume copy)",
			verb, dp.Workload.ContainerName, short(dp.NewID.String()))
		newID, err := takeover.TakeoverDB(ctx, dc, takeover.DBPlanInput{
			NewID:        dp.NewID.String(),
			Type:         dp.Row.Type,
			Version:      dp.Row.Version,
			DBUser:       dp.Row.DBUser,
			Password:     dp.Row.Password,
			RootUser:     dp.Row.RootUser,
			RootPassword: dp.Row.RootPassword,
			DefaultDB:    dp.Row.DefaultDatabase,
			PublicPort:   dp.Row.PublicPort,
			InternalPort: dp.Row.InternalPort,
			IsPublic:     dp.Row.IsPublic,
			AppendOnly:   dp.Row.AppendOnly,
			Workload:     dp.Workload,
			Start:        start,
		})
		if err != nil {
			return fmt.Errorf("db %s: %w", dp.Row.Name, err)
		}
		if !start {
			clilog.OK("%s created (status=stopped)", dp.Row.Name)
			continue
		}
		if err = takeover.WaitHealthy(ctx, dc, newID, 2*time.Minute); err != nil {
			clilog.Warn("%s: %s", dp.Row.Name, err)
		} else {
			clilog.OK("%s healthy", dp.Row.Name)
		}
	}
	return nil
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
}

func stopContainer(ctx context.Context, dc *client.Client, id string) error {
	timeout := 30
	return dc.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

func short(id string) string {
	if len(id) < 12 {
		return id
	}
	return id[:12]
}
