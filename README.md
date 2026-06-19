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

1. Installs Go if missing
2. `go install`s this migrater from source
3. **Freezes v3** — stops `coolify` + `coolify-fluentbit`, releasing port 3000 and quiescing SQLite
4. **Upgrades the host's Docker engine** (always-on — v3 ships with an old version; safe here because v3 is frozen and coolifygo isn't up yet)
5. Installs coolifygo via `gocoolify/install.sh` (skipped if already running) — port 3000 is now free and Docker is modern
6. Runs `--phase=pre-docker` — discover, extract SQLite, decrypt, plan, insert into coolifygo's Postgres
7. Runs `--phase=post-docker` — takes over every v3 workload container, then wipes v3 completely

No manual flags required — DSN + encryption key are sourced from `/data/coolifygo/.env`.

## Manual run (without install.sh)

```
coolfymigrater \
  --coolifygo-dsn=postgres://coolifygo:...@coolifygo-postgres:5432/coolifygo \
  --coolifygo-key=$(grep DATA_ENCRYPTION_KEY /data/coolifygo/.env | cut -d= -f2)
```

Both flags fall back to `$DATABASE_URL` and `$DATA_ENCRYPTION_KEY` env vars.
`--v3-secret-key` and `--v3-sqlite` are auto-detected from the running
`coolify` container; pass them if discovery fails.

| Flag | Purpose |
|---|---|
| `--phase` | `all` (default), `pre-docker` (stops after insert), or `post-docker` (resumes from state file). Used by `install.sh` to bracket the Docker upgrade. |
| `--state-file` | Where `pre-docker` writes / `post-docker` reads the plan JSON (default `/var/lib/coolfymigrater/state.json`). |
| `--dry-run` | Print the plan and exit. |
| `--yes` | Skip confirmation prompts. |
| `--no-teardown` | Keep v3 alive after data migration. |

## Scope

Built for the narrow shape the operator described:

- v3 manages **Dockerfile-built apps only** (other build packs error out).
- v3 manages **PostgreSQL + Redis databases only** (MySQL / Mongo error out).
- **No persistent storage** on application containers.
- A single **GitHub App** git source.
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
4. **extract + read** — briefly start `coolify`, `docker cp` `/app/db/prod.db`
   to a temp dir, stop again, open it via `modernc.org/sqlite`, decrypt every
   secret column with AES-256-CTR keyed by `COOLIFY_SECRET_KEY`.
5. **plan** — map v3 entities onto coolifygo row shapes. Validates that
   buildPacks and DB types are in scope.
6. **insert** — one Postgres transaction writes git_sources → applications →
   databases. Rollback on any failure leaves coolifygo untouched.
7. **takeover** — apps are recreated as `coolifygo-<slug>-<id8>` with the
   existing image + env + port binding + coolifygo's labels/network.
   Databases stop, get their data volume copied to `coolifygo-db-<id8>` via a
   one-shot alpine container, then start fresh with coolifygo's canonical
   image, env, healthcheck, and network.
8. **teardown** — stop+remove every v3 stack container (coolify, fluentbit,
   coolify-proxy), drop the seven v3 volumes (coolify-db, coolify-logs,
   coolify-local-backup, coolify-ssl-certs, two letsencrypt volumes, and the
   optional coolify-pgdb), prune `coollabsio/coolify` + `ghcr.io/coollabsio`
   images, remove the `coolify-infra` network, and clean host paths
   (`/data/coolify`, `/var/lib/coolify`, `/etc/cron.d/coolify-default`,
   `/usr/local/bin/coolify`).

## What gets copied

| v3 entity              | coolifygo target          | Notes                                          |
| ---------------------- | ------------------------- | ---------------------------------------------- |
| `GithubApp` + `GitSource` | `git_sources`          | github-app auth_type only. PEM, client secret, webhook secret re-encrypted under coolifygo's AES-256-GCM. |
| `Application` + `Secret` | `applications`         | env_vars carries decrypted secrets. Branch defaults to `main`, base_directory to `./`, dockerfile_location to `Dockerfile`. Status set to whatever the live v3 container reports. Host port carried from Docker's actual port bindings (not v3's internal port field, which was for Traefik routing). Apps with no host-published port get port 0 (no host binding). |
| `Database`             | `databases`               | Slug regenerated. Volume content copied byte-for-byte. Port + internal_port set to the canonical 5432/6379. Public port carried from Docker's actual host binding (overrides v3 SQLite if they diverge). |
| running container metadata | container_id / image_name on the new row | So the boot reconciler doesn't immediately try to recreate something that's already up. |

## What gets thrown away

- v3 management containers + their volumes + their Docker network.
- v3 host paths (`/data/coolify`, `/var/lib/coolify`, the systemd unit hint
  binary `/usr/local/bin/coolify`, the `/etc/cron.d/coolify-default` cron).
- v3 Traefik / Let's Encrypt state (SSL is out of scope in coolifygo).
- v3 backup volumes (coolifygo runs its own backup pipeline; old archives are
  not migrated).

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
