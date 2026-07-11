// Package mapper converts decrypted v3 entities into the row shapes
// coolifygo's tables expect. Pure data transform — no DB or Docker calls.
package mapper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"coolfymigrater/internal/cgo"
	"coolfymigrater/internal/discover"
	"coolfymigrater/internal/v3"

	"github.com/google/uuid"
)

// Plan is the complete migration plan after the read+map phases finish but
// before any insert or container action runs. The CLI prints it in dry-run
// mode and re-checks it once more before going forward.
type Plan struct {
	GitSources  []GitSourcePlan
	Apps        []AppPlan
	Databases   []DBPlan
	LocalServer uuid.UUID
}

// GitSourcePlan holds the inserted git_sources row payload plus the v3 source
// id so we can wire applications to the right coolifygo row after insert.
type GitSourcePlan struct {
	V3SourceID string
	Row        cgo.GitSourceRow
	NewID      uuid.UUID // filled in after insert
}

// AppPlan describes one application to insert + the v3 container to take over.
type AppPlan struct {
	Workload *discover.V3Workload // running container (nil if not running)
	V3SrcID  string               // pointer into GitSourcePlan list
	V3App    v3.Application
	Row      cgo.AppRow
	NewID    uuid.UUID
}

// DBPlan describes one database to insert + the v3 container to take over.
type DBPlan struct {
	Workload *discover.V3Workload
	V3DB     v3.Database
	Row      cgo.DBRow
	NewID    uuid.UUID
}

// BuildPlan walks v3 entities + live workload metadata and produces a Plan.
// Returns descriptive errors when the v3 stack contains anything we don't
// migrate: services, non-Dockerfile apps, persistent storages, or DB types
// outside {postgresql, redis}.
func BuildPlan(
	localServer uuid.UUID,
	apps []v3.Application,
	dbs []v3.Database,
	sources []v3.GitSource,
	ghApps []v3.GitHubApp,
	workloads []discover.V3Workload,
) (*Plan, error) {
	if len(apps) == 0 && len(dbs) == 0 {
		return nil, errors.New("nothing to migrate (no applications, no databases)")
	}

	plan := &Plan{LocalServer: localServer}

	// Validate apps: must be Dockerfile only.
	for _, a := range apps {
		bp := strings.ToLower(a.BuildPack)
		if bp != "" && bp != "dockerfile" {
			return nil, fmt.Errorf("application %q has buildPack=%q — migrater handles dockerfile only", a.Name, a.BuildPack)
		}
	}

	// Validate DBs: must be postgresql or redis.
	for _, d := range dbs {
		t := strings.ToLower(d.Type)
		if t != "postgresql" && t != "redis" {
			return nil, fmt.Errorf("database %q has type=%q — migrater handles postgresql + redis only", d.Name, d.Type)
		}
	}

	// Index github apps by id for fast lookup from git sources.
	ghByID := make(map[string]v3.GitHubApp, len(ghApps))
	for _, g := range ghApps {
		ghByID[g.ID] = g
	}

	// Git sources: only github-app rows with a usable GithubApp join.
	for _, s := range sources {
		if s.GitHubAppID == "" {
			continue
		}
		gh, ok := ghByID[s.GitHubAppID]
		if !ok || gh.AppID == 0 {
			continue
		}
		plan.GitSources = append(plan.GitSources, GitSourcePlan{
			V3SourceID: s.ID,
			Row: cgo.GitSourceRow{
				Name:          firstNonEmpty(s.Name, gh.Name, "github-app"),
				AppID:         gh.AppID,
				AppSlug:       slugify(firstNonEmpty(s.Name, gh.Name)),
				ClientID:      gh.ClientID,
				ClientSecret:  gh.ClientSecret,
				WebhookSecret: gh.WebhookSecret,
				PEM:           gh.PrivateKey,
			},
		})
	}

	// Index workloads by container-name match against v3 app name. v3's
	// container naming is roughly "<slug>-<id>" or just "<name>-<idsuffix>";
	// we match on the v3 row id (first 8 chars are commonly in the name).
	workloadByV3ID := indexWorkloads(workloads, apps, dbs)

	// Apps
	for _, a := range apps {
		webhookSecret, err := randomHex(32)
		if err != nil {
			return nil, fmt.Errorf("generate webhook_secret: %w", err)
		}
		row := cgo.AppRow{
			ServerID:           localServer,
			Name:               a.Name,
			GitRepo:            a.Repository,
			Branch:             firstNonEmpty(a.Branch, "main"),
			BuildPack:          "dockerfile",
			Port:               0,
			EnvVars:            a.Secrets,
			IsBot:              a.IsBot,
			AutoDeploy:         a.AutoDeploy,
			BaseDirectory:      firstNonEmpty(a.BaseDirectory, "./"),
			DockerfileLocation: firstNonEmpty(a.DockerFileLocation, "Dockerfile"),
			Status:             "running", // will be confirmed in takeover
			WebhookSecret:      webhookSecret,
		}
		ap := AppPlan{V3App: a, Row: row, V3SrcID: a.GitSourceID}
		if w, ok := workloadByV3ID[a.ID]; ok {
			ap.Workload = new(w)
			ap.Row.ContainerID = w.ContainerID
			ap.Row.ImageName = w.Image
			if !w.Running {
				ap.Row.Status = "stopped"
			}
			// If v3 had this container exposed on a host port, carry it over.
			// coolifygo's Port field = host binding, so we use the actual
			// published host port from Docker, not v3's internal port field.
			if hp := firstHostPort(w.PortBindings); hp > 0 {
				ap.Row.Port = hp
			}
		} else {
			ap.Row.Status = "stopped"
			ap.Row.ContainerID = ""
		}
		plan.Apps = append(plan.Apps, ap)
	}

	// Databases
	for _, d := range dbs {
		dbType := strings.ToLower(d.Type)
		password := firstNonEmpty(d.DBUserPassword, d.RootUserPassword)
		row := cgo.DBRow{
			ServerID:        localServer,
			Name:            d.Name,
			Slug:            randomSlug(),
			Type:            dbType,
			Version:         firstNonEmpty(d.Version, "latest"),
			DBUser:          d.DBUser,
			Password:        password,
			Port:            defaultPort(dbType),
			InternalPort:    defaultPort(dbType),
			RootUser:        d.RootUser,
			RootPassword:    d.RootUserPassword,
			DefaultDatabase: d.DefaultDatabase,
			IsPublic:        d.PublicPort > 0,
			PublicPort:      d.PublicPort,
			// AOF setting is meaningful only for Redis; ignore the column for
			// every other type so we don't carry junk forward.
			AppendOnly: dbType == "redis" && d.AppendOnly,
			Status:     "running",
		}
		dp := DBPlan{V3DB: d, Row: row}
		if w, ok := workloadByV3ID[d.ID]; ok {
			dp.Workload = new(w)
			dp.Row.ContainerID = w.ContainerID
			if !w.Running {
				dp.Row.Status = "stopped"
			}
			// Override public port from actual Docker host binding if present,
			// in case v3's SQLite is stale or doesn't match reality.
			if hp := firstHostPort(w.PortBindings); hp > 0 {
				dp.Row.IsPublic = true
				dp.Row.PublicPort = hp
			}
		} else {
			dp.Row.Status = "stopped"
			dp.Row.ContainerID = ""
		}
		plan.Databases = append(plan.Databases, dp)
	}

	// Port conflict check: no two resources can bind the same host port.
	// coolifygo's API enforces this at create/update time via ports.Allocator,
	// but we bypass the API, so we must catch it here.
	usedPorts := make(map[int]string) // port → resource name
	for _, ap := range plan.Apps {
		p := ap.Row.Port
		if p == 0 {
			continue
		}
		if owner, dup := usedPorts[p]; dup {
			return nil, fmt.Errorf("port %d conflict: app %q and %s both bind the same host port", p, ap.Row.Name, owner)
		}
		usedPorts[p] = fmt.Sprintf("app %q", ap.Row.Name)
	}
	for _, dp := range plan.Databases {
		if !dp.Row.IsPublic || dp.Row.PublicPort == 0 {
			continue
		}
		p := dp.Row.PublicPort
		if owner, dup := usedPorts[p]; dup {
			return nil, fmt.Errorf("port %d conflict: database %q and %s both bind the same host port", p, dp.Row.Name, owner)
		}
		usedPorts[p] = fmt.Sprintf("database %q", dp.Row.Name)
	}

	return plan, nil
}

// ValidatePortsAgainst checks the plan's ports against an existing set of
// used ports from coolifygo's database. Returns an error on first conflict.
func (p *Plan) ValidatePortsAgainst(existing map[int]string) error {
	for _, ap := range p.Apps {
		if ap.Row.Port == 0 {
			continue
		}
		if owner, dup := existing[ap.Row.Port]; dup {
			return fmt.Errorf("port %d conflict: migrating app %q would collide with %s already in coolifygo", ap.Row.Port, ap.Row.Name, owner)
		}
	}
	for _, dp := range p.Databases {
		if !dp.Row.IsPublic || dp.Row.PublicPort == 0 {
			continue
		}
		if owner, dup := existing[dp.Row.PublicPort]; dup {
			return fmt.Errorf("port %d conflict: migrating database %q would collide with %s already in coolifygo", dp.Row.PublicPort, dp.Row.Name, owner)
		}
	}
	return nil
}

// SetGitSourceID writes the inserted git_sources UUID back into every AppPlan
// that referenced the same v3 source id. Called by the CLI after the inserts
// for git_sources complete inside the migration transaction.
func (p *Plan) SetGitSourceID(v3SourceID string, newID uuid.UUID) {
	idStr := newID.String()
	for i := range p.GitSources {
		if p.GitSources[i].V3SourceID == v3SourceID {
			p.GitSources[i].NewID = newID
		}
	}
	for i := range p.Apps {
		if p.Apps[i].V3SrcID == v3SourceID {
			p.Apps[i].Row.GitSourceID = idStr
		}
	}
}

func indexWorkloads(workloads []discover.V3Workload, apps []v3.Application, dbs []v3.Database) map[string]discover.V3Workload {
	out := make(map[string]discover.V3Workload, len(workloads))
	// v3 container names look like "<name-or-slug>-<id8>" where id8 is the
	// first 8 chars of the cuid. We match by that suffix.
	byShort := make(map[string]discover.V3Workload, len(workloads))
	for _, w := range workloads {
		if w.ContainerName == "" {
			continue
		}
		parts := strings.Split(w.ContainerName, "-")
		short := parts[len(parts)-1]
		byShort[short] = w
	}
	for _, a := range apps {
		short := safeShort(a.ID)
		if w, ok := byShort[short]; ok {
			out[a.ID] = w
		}
	}
	for _, d := range dbs {
		short := safeShort(d.ID)
		if w, ok := byShort[short]; ok {
			out[d.ID] = w
		}
	}
	return out
}

func safeShort(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// firstHostPort returns the lowest host port from a workload's port bindings,
// or 0 if none are published. Deterministic so dry-run plans don't shuffle
// across invocations when a container has multiple published ports.
func firstHostPort(bindings map[int]int) int {
	lowest := 0
	for hp := range bindings {
		if lowest == 0 || hp < lowest {
			lowest = hp
		}
	}
	return lowest
}

func defaultPort(dbType string) int {
	switch dbType {
	case "postgresql":
		return 5432
	case "redis":
		return 6379
	}
	return 0
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

func randomHex(n int) (string, error) {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomSlug returns a 12-char lowercase hex slug, matching coolifygo's
// Database.Slug convention (handle used in connection strings / env vars).
func randomSlug() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
