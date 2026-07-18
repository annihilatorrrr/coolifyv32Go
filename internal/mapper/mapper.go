// Package mapper converts decrypted v3 entities into the row shapes
// coolifygo's tables expect. Pure data transform — no DB or Docker calls.
package mapper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/annihilatorrrr/coolifyv32Go/internal/cgo"
	"github.com/annihilatorrrr/coolifyv32Go/internal/discover"
	"github.com/annihilatorrrr/coolifyv32Go/internal/v3"

	"github.com/google/uuid"
)

// Plan is the complete migration plan after the read+map phases finish but
// before any insert or container action runs. The CLI prints it in dry-run
// mode and re-checks it once more before going forward.
type Plan struct {
	GitSources      []GitSourcePlan
	Apps            []AppPlan
	Databases       []DBPlan
	OrphanWorkloads []OrphanWorkload
	GitLinkWarnings []string
	LocalServer     uuid.UUID
}

// OrphanWorkload is a v3-managed container discovered on the host that no
// migrated app or database claimed. A non-empty set means the migration is
// incomplete: teardown refuses to wipe v3 while any exist, so a container whose
// row match silently missed is never stranded or destroyed (the exact failure
// that left running bots orphaned after an earlier run).
type OrphanWorkload struct {
	ContainerID string
	Name        string
	Image       string
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

	// Validate apps: must be a Dockerfile build. v3 names that build pack
	// "docker" (see coolify v3 buildPacks/docker.ts — "dockerfile" is a
	// coolifygo-side value, never a v3 one). Everything else — nixpacks,
	// static, node, php, and crucially "compose" (docker-compose) — is out of
	// scope and bails here.
	for _, a := range apps {
		bp := strings.ToLower(a.BuildPack)
		if bp != "" && bp != "docker" && bp != "dockerfile" {
			return nil, fmt.Errorf("application %q has buildPack=%q — migrater handles Dockerfile builds only (v3 buildPack \"docker\")", a.Name, a.BuildPack)
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

	// Git sources: only github-app rows with a usable GithubApp join. coolifygo
	// needs exactly app_id + pem (JWT signing) + client_id/client_secret/
	// webhook_secret (webhooks); it hits api.github.com directly and resolves
	// the installation per-repo at deploy time, so no base_url / installation_id
	// is stored. importedSrc lets us flag apps below whose git source we could
	// NOT import, rather than silently dropping their git integration.
	importedSrc := make(map[string]bool, len(sources))
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
		importedSrc[s.ID] = true
	}

	// Index live workloads back to their v3 row id. v3 names every managed
	// container by the raw row cuid and additionally stamps apps with a
	// coolify.applicationId label — see indexWorkloads for the exact matching.
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
		// Surface a lost git link rather than importing it silently: an app that
		// referenced a v3 git source we couldn't turn into a coolifygo GitHub App
		// (missing/incomplete GithubApp row) is migrated without git integration,
		// so auto-deploy / rebuild-on-webhook won't work until it's re-linked.
		if a.GitSourceID != "" && !importedSrc[a.GitSourceID] {
			plan.GitLinkWarnings = append(plan.GitLinkWarnings,
				fmt.Sprintf("app %q references a git source with no usable GitHub App — imported without git integration (auto-deploy/rebuild off until re-linked)", a.Name))
		}
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
			// A published host port also means the app is genuinely exposed, so
			// it cannot be a coolifygo "bot/worker": is_bot=true suppresses the
			// host bind in BOTH takeover and coolifygo's BuildAppContainerConfig
			// (both gate on Port>0 && !IsBot). Leaving is_bot=true with port>0
			// would persist a row whose App URL advertises a port nothing binds
			// — reachable until the first recreate (restart/reconcile), then
			// ERR_CONNECTION_REFUSED. Reconcile the pair, mirroring the
			// IsPublic/PublicPort handling for databases below: an exposed app is
			// never a bot, regardless of v3's isBot flag.
			if hp := firstHostPort(w.PortBindings); hp > 0 {
				ap.Row.Port = hp
				ap.Row.IsBot = false
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

	// Completeness guard: every discovered v3-managed workload should map to a
	// migrated app or database. Any that don't are recorded as orphans so the
	// caller can refuse teardown — this is exactly the case that strands a
	// running container (e.g. a bot whose row match silently missed) and would
	// otherwise be destroyed when v3 is wiped.
	matched := make(map[string]bool, len(plan.Apps)+len(plan.Databases))
	for i := range plan.Apps {
		if w := plan.Apps[i].Workload; w != nil {
			matched[w.ContainerID] = true
		}
	}
	for i := range plan.Databases {
		if w := plan.Databases[i].Workload; w != nil {
			matched[w.ContainerID] = true
		}
	}
	for i := range workloads {
		if w := workloads[i]; !matched[w.ContainerID] {
			plan.OrphanWorkloads = append(plan.OrphanWorkloads, OrphanWorkload{
				ContainerID: w.ContainerID,
				Name:        w.ContainerName,
				Image:       w.Image,
			})
		}
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

// AppContainerName is the stable coolifygo container name for an application,
// identical to coolifygo's deploy.ContainerName ("coolifygo-<slug>-<id8>").
// Single source of truth for both the "already adopted?" check and adoption
// targets in --oldfix.
func AppContainerName(id uuid.UUID, name string) string {
	return "coolifygo-" + slugify(name) + "-" + id.String()[:8]
}

// AdoptCandidate is a live, still-running container left behind by an
// interrupted takeover (a v3 workload under its old name). HostPort is the
// lowest published host port (0 if none), read from the live Docker binding —
// the same host-port semantics coolifygo's Port column uses.
type AdoptCandidate struct {
	ID       string
	Name     string
	Image    string
	HostPort int
}

// AdoptTarget pairs a coolifygo application row with the live container that
// should be renamed into it.
type AdoptTarget struct {
	AppName   string
	NewName   string
	LiveID    string
	LiveImage string
	HostPort  int
	AppID     uuid.UUID
}

// MatchAdoptable pairs orphaned application rows with leftover live containers.
// Pure — no IO. Two passes:
//
//  1. Deterministic: the row's seeded container_id points straight at a live
//     container (the insert phase stamps v3's id onto the row when it matched a
//     workload). No name guessing — this wins, because v3 names workload
//     containers by raw cuid, which rarely equals the human app name.
//  2. Bijective name match on the leftovers. An app matching zero or many
//     candidates, or a candidate claimed by many apps, is reported in unmatched
//     and left untouched.
//
// Safety over cleverness — the caller confirms interactively before renaming
// anything, and prints the expected container name for whatever stays unmatched
// so the operator can rename by hand and re-run.
func MatchAdoptable(apps []cgo.AppInfo, candidates []AdoptCandidate) (targets []AdoptTarget, unmatched []string) {
	claimed := make([]bool, len(candidates))
	matched := make([]bool, len(apps))

	// Pass 1 — container_id match.
	byID := make(map[string]int, len(candidates))
	for i := range candidates {
		byID[candidates[i].ID] = i
	}
	for ai := range apps {
		if apps[ai].ContainerID == "" {
			continue
		}
		if ci, ok := byID[apps[ai].ContainerID]; ok && !claimed[ci] {
			claimed[ci], matched[ai] = true, true
			targets = append(targets, adoptTarget(apps[ai], candidates[ci]))
		}
	}

	// Pass 2 — bijective name match among unmatched apps and unclaimed candidates.
	appHits := make([][]int, len(apps))
	candHits := make([][]int, len(candidates))
	for ai := range apps {
		if matched[ai] {
			continue
		}
		for ci := range candidates {
			if claimed[ci] {
				continue
			}
			if nameMatch(apps[ai].Name, candidates[ci].Name) {
				appHits[ai] = append(appHits[ai], ci)
				candHits[ci] = append(candHits[ci], ai)
			}
		}
	}
	for ai := range apps {
		if matched[ai] {
			continue
		}
		a := apps[ai]
		switch len(appHits[ai]) {
		case 0:
			unmatched = append(unmatched, fmt.Sprintf("app %q: no live container matched", a.Name))
			continue
		case 1:
		default:
			unmatched = append(unmatched, fmt.Sprintf("app %q: %d live containers matched (ambiguous)", a.Name, len(appHits[ai])))
			continue
		}
		ci := appHits[ai][0]
		if len(candHits[ci]) != 1 {
			unmatched = append(unmatched, fmt.Sprintf("app %q: container %q also matches other apps (ambiguous)", a.Name, candidates[ci].Name))
			continue
		}
		targets = append(targets, adoptTarget(a, candidates[ci]))
	}
	return targets, unmatched
}

func adoptTarget(a cgo.AppInfo, c AdoptCandidate) AdoptTarget {
	return AdoptTarget{
		AppID:     a.ID,
		AppName:   a.Name,
		NewName:   AppContainerName(a.ID, a.Name),
		LiveID:    c.ID,
		LiveImage: c.Image,
		HostPort:  c.HostPort,
	}
}

// nameMatch reports whether a v3 container name refers to the given app. v3
// names workload containers by the raw cuid, but user-facing installs commonly
// surface "<name>" or "<name>-<cuid>", so we accept an exact match, a dash/
// underscore segment equal to the app name, or a slugified "<name>-"/"<name>_"
// prefix. Case-insensitive throughout.
func nameMatch(appName, containerName string) bool {
	a := strings.ToLower(strings.TrimSpace(appName))
	c := strings.ToLower(strings.TrimSpace(containerName))
	if a == "" || c == "" {
		return false
	}
	if a == c {
		return true
	}
	if slices.Contains(strings.FieldsFunc(c, func(r rune) bool { return r == '-' || r == '_' }), a) {
		return true
	}
	s := slugify(a)
	return strings.HasPrefix(c, s+"-") || strings.HasPrefix(c, s+"_")
}

func indexWorkloads(workloads []discover.V3Workload, apps []v3.Application, dbs []v3.Database) map[string]discover.V3Workload {
	out := make(map[string]discover.V3Workload, len(workloads))

	// A v3 workload exposes its row cuid in several places; index every signal
	// so matching is robust to naming quirks:
	//   - apps carry the cuid in the coolify.applicationId label (set by both
	//     makeLabelForSimpleDockerfile and makeLabelForStandaloneApplication);
	//   - both apps and databases name the container by the raw cuid
	//     (container_name == applicationId / database id — no slug, no dashes);
	//   - legacy "<slug>-<id8>" style names only surface the id as the trailing
	//     dash segment, so we also index by an 8-char prefix as a fallback.
	// Full-id matches win over 8-char-prefix matches (two cuids created close
	// in time can share a prefix), so the two maps stay separate and full is
	// consulted first.
	byFull := make(map[string]discover.V3Workload, len(workloads))
	byShort := make(map[string]discover.V3Workload, len(workloads))
	add := func(token string, w discover.V3Workload) {
		if token == "" {
			return
		}
		if _, ok := byFull[token]; !ok {
			byFull[token] = w
		}
		if len(token) >= 8 {
			if _, ok := byShort[token[:8]]; !ok {
				byShort[token[:8]] = w
			}
		}
	}
	for _, w := range workloads {
		add(w.Labels["coolify.applicationId"], w)
		add(w.ContainerName, w)
		if w.ContainerName != "" {
			parts := strings.Split(w.ContainerName, "-")
			add(parts[len(parts)-1], w)
		}
	}

	match := func(id string) (discover.V3Workload, bool) {
		if id == "" {
			return discover.V3Workload{}, false
		}
		if w, ok := byFull[id]; ok {
			return w, true
		}
		if len(id) >= 8 {
			if w, ok := byShort[id[:8]]; ok {
				return w, true
			}
		}
		return discover.V3Workload{}, false
	}
	for _, a := range apps {
		if w, ok := match(a.ID); ok {
			out[a.ID] = w
		}
	}
	for _, d := range dbs {
		if w, ok := match(d.ID); ok {
			out[d.ID] = w
		}
	}
	return out
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
