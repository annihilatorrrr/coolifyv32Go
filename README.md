# coolfymigrater

One-shot migration tool that hands a running Coolify v3 install over to
coolifygo on the same host. Reads v3's SQLite database directly, decrypts its
secrets with v3's `COOLIFY_SECRET_KEY`, inserts equivalent rows into
coolifygo's Postgres, then takes over every workload container (apps + DBs)
and finally wipes the v3 stack.

## Scope

Built for the narrow shape the operator described:

- v3 manages **Dockerfile-built apps only** (other build packs error out).
- v3 manages **PostgreSQL + Redis databases only** (MySQL / Mongo error out).
- **No persistent storage** on application containers.
- A single **GitHub App** git source.
- v3 runs on the **same host** as coolifygo (local Docker socket).

Everything else (services, FQDN/Traefik/SSL, teams, PR previews, remote
destinations, Storages) is intentionally skipped.

## How it runs

```
coolfymigrater \
  --coolifygo-dsn=postgres://coolifygo:...@coolifygo-postgres:5432/coolifygo \
  --coolifygo-key=$(cat /data/coolifygo/.env | grep DATA_ENCRYPTION_KEY | cut -d= -f2)
```

Both flags fall back to `$DATABASE_URL` and `$DATA_ENCRYPTION_KEY` env vars.
`--v3-secret-key` and `--v3-sqlite` are auto-detected from the running
`coolify` container; pass them if discovery fails. `--dry-run` prints the plan
and exits. `--yes` skips confirmation prompts. `--no-teardown` keeps v3 alive
on disk after the data migration.

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
| `Application` + `Secret` | `applications`         | env_vars carries decrypted secrets. Branch defaults to `main`, base_directory to `./`, dockerfile_location to `Dockerfile`. Status set to whatever the live v3 container reports. |
| `Database`             | `databases`               | Slug regenerated. Volume content copied byte-for-byte. Port + internal_port set to the canonical 5432/6379. |
| running container metadata | container_id / image_name on the new row | So the boot reconciler doesn't immediately try to recreate something that's already up. |

## What gets thrown away

- v3 management containers + their volumes + their Docker network.
- v3 host paths (`/data/coolify`, `/var/lib/coolify`, the systemd unit hint
  binary `/usr/local/bin/coolify`, the `/etc/cron.d/coolify-default` cron).
- v3 Traefik / Let's Encrypt state (SSL is out of scope in coolifygo).
- v3 backup volumes (coolifygo runs its own backup pipeline; old archives are
  not migrated).

## Safety

- Inserts run inside a single Postgres transaction. Rollback on any failure
  leaves coolifygo in its pre-migration state.
- Container takeover begins only after the transaction commits. A failed
  takeover leaves a partial state in coolifygo but never destroys v3's data
  volumes (those are dropped by the teardown phase, gated by a confirm prompt
  unless `--yes` is passed).
- `--dry-run` prints the full plan and exits before any change.

## Re-runs

The migrater is idempotent on names: applications, databases, and GitHub Apps
that already exist in coolifygo are skipped. Container takeover is safe to
re-run because new container creation removes any prior `coolifygo-…` with
the same name first.
