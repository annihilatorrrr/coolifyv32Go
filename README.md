# coolfymigrater

One-shot migration tool that hands a running Coolify v3 install over to
**coolifygo** on the same host. Reads v3's SQLite database directly, decrypts
its secrets with v3's `COOLIFY_SECRET_KEY`, inserts equivalent rows into
coolifygo's Postgres, takes over every workload container (apps + DBs),
upgrades the host Docker engine, and finally wipes the v3 stack.

## Quickstart — one command

On the v3 host:

```bash
curl -fsSL https://raw.githubusercontent.com/annihilatorrrr/coolifyv32Go/main/install.sh | sudo bash
```

That wrapper does the whole thing end to end:

1. Installs Go if missing or older than required (Go 1.26.5+) — never downgrades a newer host toolchain
2. `go install`s this migrater from source
3. **Freezes v3** — stops `coolify` + `coolify-fluentbit`, releasing port 3000 and quiescing SQLite
4. **Upgrades the host's Docker engine** (always-on — v3 ships with an old version; safe here because v3 is frozen and coolifygo isn't up yet)
5. Installs coolifygo via `gocoolify/install.sh` (skipped if already running) — port 3000 is now free and Docker is modern
6. Runs `--phase=pre-docker` — discover, extract SQLite, decrypt, plan, insert into coolifygo's Postgres
7. Runs `--phase=post-docker` — takes over every v3 workload container, then wipes v3 completely

No manual flags required — DSN + encryption key are sourced from `/data/coolifygo/.env`.

## Install

The migrater is a single Go binary. You can install it the same way
`install.sh` does, or build it from source.

```bash
# Install the published binary onto PATH (what install.sh runs internally)
GOBIN=/usr/local/bin go install github.com/annihilatorrrr/coolifyv32Go@latest

# …or build from a checkout of this repo
git clone https://github.com/annihilatorrrr/coolifyv32Go
cd coolifyv32Go
go build -o bin/coolfymigrater .      # binary at ./bin/coolfymigrater
```

Requires Go 1.26.5+ and a local Docker socket. No CGO — the SQLite reader is
pure Go (`modernc.org/sqlite`).

## Usage

```
coolfymigrater [flags]
```

The two connection flags are the only required inputs. Everything about the
v3 side (`COOLIFY_SECRET_KEY`, the `prod.db` path, the workload containers) is
auto-detected from the running `coolify` container — you never supply those.

#### Where do the DSN and key come from?

You don't invent them — **coolifygo writes them to its own env file when it is
installed**, at `/data/coolifygo/.env`:

```
DATABASE_URL=postgres://coolifygo:...@coolifygo-postgres:5432/coolifygo
DATA_ENCRYPTION_KEY=<base64 of 32 random bytes>
```

- **Via `install.sh` (the one-command path): you do nothing.** The wrapper
  sources `/data/coolifygo/.env` for you (`load_coolifygo_env`) and passes both
  values through — no flags, no exports.
- **Running the binary by hand:** source that same file first so the two flags
  pick the values up from the environment:

  ```bash
  set -a; . /data/coolifygo/.env; set +a   # exports DATABASE_URL + DATA_ENCRYPTION_KEY
  coolfymigrater --yes
  ```

  or point at it explicitly without exporting anything:

  ```bash
  coolfymigrater \
    --coolifygo-dsn="$(grep '^DATABASE_URL='        /data/coolifygo/.env | cut -d= -f2-)" \
    --coolifygo-key="$(grep '^DATA_ENCRYPTION_KEY=' /data/coolifygo/.env | cut -d= -f2-)"
  ```

(The file path is configurable in `install.sh` via `COOLIFYGO_ENV`.)

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--coolifygo-dsn` | `$DATABASE_URL` | **Required.** coolifygo Postgres DSN to write the migrated rows into. |
| `--coolifygo-key` | `$DATA_ENCRYPTION_KEY` | **Required.** coolifygo `DATA_ENCRYPTION_KEY` — base64 of 32 raw bytes. Secrets are re-encrypted under this key (AES-256-GCM, `enc:v1:` prefix). |
| `--v3-secret-key` | auto-detected | v3 `COOLIFY_SECRET_KEY` used to decrypt v3's secret columns. Read from the `coolify` container's env when blank; pass it explicitly only if that detection fails. |
| `--v3-sqlite` | auto-extracted | Path to v3's `prod.db` on the host. When blank, it is `docker cp`'d out of the (frozen) `coolify` container to a temp dir and cleaned up afterwards. |
| `--phase` | `all` | `all` (single-shot), `pre-docker` (discover → insert, then save state and exit), or `post-docker` (reload state → takeover → teardown). Used by `install.sh` to bracket the Docker-engine upgrade. |
| `--state-file` | `/var/lib/coolfymigrater/state.json` | Where `pre-docker` writes / `post-docker` reads the plan JSON. |
| `--dry-run` | `false` | Print the full plan and exit before any change. |
| `--yes` | `false` | Skip the interactive confirmation prompts (proceed + teardown). |
| `--no-teardown` | `false` | Keep the v3 stack alive after the data migration + takeover complete. |
| `--oldfix` | `false` | **Repair mode.** Adopt workload containers a previous interrupted takeover left running under their old v3 names — no v3 SQLite, state file, or image rebuild needed. See [Repair mode](#repair-mode---oldfix). |

### Environment variables

| Variable | Equivalent flag | Where it comes from |
|---|---|---|
| `DATABASE_URL` | `--coolifygo-dsn` | Written by coolifygo into `/data/coolifygo/.env` at install. |
| `DATA_ENCRYPTION_KEY` | `--coolifygo-key` | Written by coolifygo into `/data/coolifygo/.env` at install. |
| `DOCKER_HOST` / `DOCKER_*` | — | Standard Docker SDK env — points the client at the local daemon socket (defaults to the local socket). |

### Examples

```bash
# 1. Dry run — see exactly what would be migrated, change nothing.
coolfymigrater --coolifygo-dsn=$DATABASE_URL --coolifygo-key=$DATA_ENCRYPTION_KEY --dry-run

# 2. Full single-shot migration, no prompts (env vars already exported).
coolfymigrater --yes

# 3. Migrate the data + take over containers, but LEAVE v3 running
#    (nothing is wiped — useful to validate coolifygo before committing).
coolfymigrater --yes --no-teardown

# 4. Provide v3 secrets explicitly if auto-detection can't reach the container.
coolfymigrater \
  --v3-secret-key=base64:xxxxxxxx... \
  --v3-sqlite=/data/coolify/source/prod.db

# 5. Phase-split run by hand (what install.sh does around a Docker upgrade).
coolfymigrater --phase=pre-docker  --state-file=/tmp/mig.json --yes
#   … upgrade Docker / restart the daemon here …
coolfymigrater --phase=post-docker --state-file=/tmp/mig.json --yes

# 6. Repair a half-finished takeover (containers still up under old v3 names).
#    Needs only the coolifygo connection — v3 can be long gone.
set -a; . /data/coolifygo/.env; set +a
coolfymigrater --oldfix
```

> The connection flags (`--coolifygo-dsn` / `--coolifygo-key`) are required in
> **every** phase, including `post-docker` — takeover reopens Postgres to write
> the real container ids back onto the migrated rows.

### `install.sh` overrides

The wrapper is driven entirely by environment variables, so you can pin
versions or point it at forks without editing the script:

```bash
MIGRATER_REF=v1.2.3 sudo -E bash install.sh
```

| Variable | Default | Purpose |
|---|---|---|
| `GO_VERSION` | `1.26.5` | Minimum Go required (matches go.mod) and the version installed when the host is short of it. A host with this version or newer is used untouched — never downgraded. |
| `GO_INSTALL_DIR` | `/usr/local/go` | Where the Go tarball is unpacked (removed on exit if the script installed it). |
| `COOLIFYGO_INSTALL_URL` | gocoolify `install.sh` | Installer used to bring up coolifygo when it isn't already running. |
| `MIGRATER_MODULE` | `github.com/annihilatorrrr/coolifyv32Go` | Module path passed to `go install`. |
| `MIGRATER_REF` | `latest` | Git ref / version to install (`@<ref>`). `go install` runs with `GOPROXY=direct` so it always fetches from GitHub, bypassing the module proxy cache. |
| `STATE_FILE` | `/var/lib/coolfymigrater/state.json` | Passed through as `--state-file` to both phases. |
| `COOLIFYGO_ENV` | `/data/coolifygo/.env` | File sourced for `DATABASE_URL` + `DATA_ENCRYPTION_KEY`. |
| `ASSUME_YES` | `1` | When `1`, passes `--yes` to the binary so `curl \| bash` runs unattended. Set to `0` to keep the prompts. |

## Build & test

```bash
go build ./...                    # compile everything
go build -o bin/coolfymigrater .  # build the CLI binary
go vet ./...
go test ./...
```

## Scope

Built for the narrow shape the operator described:

- v3 manages **Dockerfile-built apps only** (other build packs error out).
- v3 manages **PostgreSQL + Redis databases only** (MySQL / Mongo error out).
- **No persistent storage** on application containers.
- **GitHub App** git sources only — other git-source auth types are skipped
  (multiple GitHub App sources are supported).
- v3 runs on the **same host** as coolifygo (local Docker socket).

Everything else (services, FQDN/Traefik/SSL, teams, PR previews, remote
destinations, Storages) is intentionally skipped.

## Phases

1. **discover** — inspect the local `coolify` container for the secret key and
   container metadata; list every workload on the `coolify-infra` network.
2. **connect coolifygo postgres** — verify DSN + key + that a `type=local`
   server row already exists (created by coolifygo's `EnsureLocalServer`).
3. **freeze** — stop `coolify` and `coolify-fluentbit` so v3's SQLite is in a
   consistent state.
4. **extract + read** — `docker cp` `/app/db/prod.db` out of the already-frozen
   `coolify` container (no restart needed — `docker cp` works on stopped
   containers and the file is consistent because v3 is quiesced). Opens the
   copy via `modernc.org/sqlite`, decrypts every secret column with
   AES-256-CTR keyed by `COOLIFY_SECRET_KEY`. Temp file is cleaned up after
   the read.
5. **plan** — map v3 entities onto coolifygo row shapes. Validates that
   buildPacks and DB types are in scope.
6. **insert** — one Postgres transaction writes git_sources → applications →
   databases. Rollback on any failure leaves coolifygo untouched.
7. **takeover** — apps are recreated as `coolifygo-<slug>-<id8>` on the
   `coolifygo` bridge network with the existing image + env + host port
   binding + coolifygo's `coolifygo.managed` label. Databases stop, get their
   data volume **copied** (not moved) to `coolifygo-db-<id8>` via a one-shot
   `alpine:3` container, then start fresh as `coolifygo-db-<id8>` with
   coolifygo's canonical image (`postgres:<v>-alpine` / `redis:<v>-alpine`),
   env, healthcheck, and network. After each recreate the real new container
   id + status are written back onto the coolifygo row.
8. **teardown** — stop+remove every v3 stack container (`coolify`,
   `coolify-fluentbit`, `coolify-proxy`, `coolify-haproxy`); drop the seven v3
   infra volumes (`coolify-db`, `coolify-logs`, `coolify-local-backup`,
   `coolify-ssl-certs`, two letsencrypt volumes, and the optional
   `coolify-pgdb`); reclaim each **copied-from database source volume** (the
   originals takeover copied out of — guarded: never touches a `coolifygo-`
   volume, `force=false` so an in-use one is reported, not destroyed); prune
   `coollabsio/coolify`, `ghcr.io/coollabsio/coolify`, and
   `ghcr.io/coollabsio/fluent-bit` images plus the migrater's own `alpine:3`
   copy-helper (only if unused); remove the `coolify-infra` network; and clean
   host paths (`/data/coolify`, `/var/lib/coolify`, `/etc/cron.d/coolify-default`,
   `/usr/local/bin/coolify`). On a completed `post-docker` run the migrater
   also removes its own state file and the now-empty state directory.

## What gets copied

| v3 entity              | coolifygo target          | Notes                                          |
| ---------------------- | ------------------------- | ---------------------------------------------- |
| `GithubApp` + `GitSource` | `git_sources`          | github-app auth_type only. PEM, client secret, webhook secret re-encrypted under coolifygo's AES-256-GCM. |
| `Application` + `Secret` | `applications`         | env_vars carries decrypted secrets. Branch defaults to `main`, base_directory to `./`, dockerfile_location to `Dockerfile`. Status set to whatever the live v3 container reports. Host port carried from Docker's actual port bindings (not v3's internal port field, which was for Traefik routing). Apps with no host-published port get port 0 (no host binding). |
| `Database`             | `databases`               | Slug regenerated. Volume content copied byte-for-byte. Port + internal_port set to the canonical 5432/6379. Public port carried from Docker's actual host binding (overrides v3 SQLite if they diverge). |
| running container metadata | container_id / image_name on the new row | So the boot reconciler doesn't immediately try to recreate something that's already up. |

## What gets thrown away

- v3 management containers (`coolify`, `coolify-fluentbit`, `coolify-proxy`,
  `coolify-haproxy`) + their infra volumes + the `coolify-infra` network.
- The original v3 **database source volumes** — reclaimed after their contents
  were copied into the new `coolifygo-db-<id8>` volumes.
- v3 host paths (`/data/coolify`, `/var/lib/coolify`, the systemd unit hint
  binary `/usr/local/bin/coolify`, the `/etc/cron.d/coolify-default` cron).
- v3 Traefik / Let's Encrypt state (SSL is out of scope in coolifygo).
- v3 backup volumes (coolifygo runs its own backup pipeline; old archives are
  not migrated).
- `coollabsio/*` + `ghcr.io/coollabsio/*` images, and the migrater's own
  `alpine:3` copy-helper (only when no other container still uses it).

## Safety

- **Port conflict detection** at plan time: the migrater checks for duplicate
  host ports within the migration batch and against resources already in
  coolifygo's database. Bails with a descriptive error before any change.
- Inserts run inside a single Postgres transaction. Rollback on any failure
  leaves coolifygo in its pre-migration state.
- Container takeover begins only after the transaction commits. A failed
  takeover leaves a partial state in coolifygo but never destroys v3's data
  volumes (those are dropped by the teardown phase, gated by a confirm prompt
  unless `--yes` is passed).
- **Completeness guard on teardown.** Every discovered v3-managed container must
  map to a migrated app/DB. If any go unmatched (an *orphan* — the failure mode
  that strands a running workload), teardown is **refused** and v3 is left fully
  intact so nothing is stranded or destroyed. This overrides `--yes` on purpose:
  v3 is never wiped while the picture is partial. The unmatched containers are
  listed in the plan output; investigate, fix the mapping, and re-run — or use
  `--oldfix` afterward to adopt whatever the takeover left behind.
- **Git-link visibility.** An application that referenced a v3 git source the
  migrater couldn't turn into a usable coolifygo GitHub App is imported without
  git integration (auto-deploy/rebuild off) and flagged loudly in the plan —
  never dropped silently.
- `--dry-run` prints the full plan and exits before any change.

## Re-runs

- **Post-docker phase** can be resumed from the saved state file
  (`/var/lib/coolfymigrater/state.json` by default). `stopAndRemove` tolerates
  containers that were already cleaned up on a prior attempt, so an interrupted
  takeover can be picked up where it left off.
- **Pre-docker phase** is **not** idempotent — coolifygo's schema has no
  unique constraint on names, so re-inserting would create duplicate rows.
  Drop coolifygo's database (greenfield design) before re-running pre-docker
  from scratch.
- A fully successful post-docker run deletes the state file so a stale resume
  can't confuse a future invocation.

## Repair mode (`--oldfix`)

If a takeover was interrupted — or an earlier bug left a workload unmatched —
you can end up with coolifygo holding the application rows while the actual
containers are still running under their **old v3 names**. coolifygo then shows
those apps as stopped / container-less even though the process never stopped.
The normal `post-docker` resume can't help once v3 is gone (no SQLite, no state
file), so `--oldfix` is a standalone repair that works from **only** coolifygo's
Postgres and the live Docker daemon:

```bash
set -a; . /data/coolifygo/.env; set +a   # DATABASE_URL + DATA_ENCRYPTION_KEY
coolfymigrater --oldfix                  # review the proposed adoptions, then confirm
```

For each application row whose expected `coolifygo-<slug>-<id8>` container isn't
running, it finds the leftover live container and **adopts** it. Only genuine
coolify v3 leftovers are ever considered — a candidate must carry v3's
`coolify.managed=true` label, so an unrelated host container that merely shares
a name with an app row is never touched. Adoption then:

- **Renames** it to the coolifygo name the reconciler expects — same container,
  **zero downtime**, no image rebuild or pull.
- Attaches it to the `coolifygo` network (best-effort — coolifygo manages by
  name + id regardless).
- Writes the live **container id**, **image**, **host port**, and
  `status=running` back onto the row, so Stop/Logs/Stats target the live
  container and the reconciler can heal it later.

It is safe and re-runnable: matches are bijective and unambiguous (anything
uncertain is reported and left untouched), it prompts before changing anything
(skip with `--yes`), and it is a **no-op** when every row already maps to a
running container. It requires `--coolifygo-dsn` + `--coolifygo-key` like every
other mode. Scope is **applications only** — a database whose data volume was
already reclaimed at teardown can't be re-homed by a rename.
