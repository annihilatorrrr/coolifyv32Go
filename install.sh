#!/usr/bin/env bash
# coolfymigrater — one-shot wrapper that turns a host running Coolify v3 into
# a host running coolifygo with every app + database migrated, then wipes v3.
#
# Operator usage:
#   curl -fsSL https://raw.githubusercontent.com/annihilatorrrr/coolifyv32Go/main/install.sh | sudo bash
#
# The wrapper:
#   1. Installs Go (only if missing or older than required) at /usr/local/go
#      using the official tarball — never downgrades a newer host toolchain.
#   2. `go install`s the coolfymigrater binary from this repo's main branch.
#   3. Freezes v3 (stops `coolify` + `coolify-fluentbit`) — releases :3000 so
#      coolifygo can bind it, and quiesces SQLite for extraction.
#   4. Upgrades the host Docker engine (always-on; v3 ships with an old version).
#      Safe here because v3 is already frozen and coolifygo isn't installed yet,
#      so the dockerd restart disrupts nothing live.
#   5. Installs coolifygo via the gocoolify install.sh — skips if already up.
#      gocoolify's installer only installs Docker if missing; never upgrades
#      existing Docker. That's why step 4 has to happen here, not inside it.
#   6. Runs the binary `--phase=pre-docker` (discover → idempotent re-freeze →
#      SQLite extract → read → plan → insert into coolifygo's Postgres).
#   7. Runs the binary `--phase=post-docker` (takeover → wipe v3).
#
# Idempotent on re-runs: each step probes for existing state before acting,
# so a failed run can be re-launched without compounding damage.

set -Eeuo pipefail

# ── Config ────────────────────────────────────────────────────────────────
GO_VERSION="${GO_VERSION:-1.26.5}"
GO_INSTALL_DIR="${GO_INSTALL_DIR:-/usr/local/go}"
COOLIFYGO_INSTALL_URL="${COOLIFYGO_INSTALL_URL:-https://raw.githubusercontent.com/annihilatorrrr/gocoolify/main/install.sh}"
MIGRATER_MODULE="${MIGRATER_MODULE:-github.com/annihilatorrrr/coolifyv32Go}"
MIGRATER_REF="${MIGRATER_REF:-latest}"
STATE_FILE="${STATE_FILE:-/var/lib/coolfymigrater/state.json}"
COOLIFYGO_ENV="${COOLIFYGO_ENV:-/data/coolifygo/.env}"
ASSUME_YES="${ASSUME_YES:-1}"   # 1 = pass --yes to the binary so curl|bash works unattended

# ── Plumbing ──────────────────────────────────────────────────────────────
RED=$'\e[31m'; GREEN=$'\e[32m'; YELLOW=$'\e[33m'; BLUE=$'\e[34m'; RESET=$'\e[0m'
info()  { printf '%s>>%s %s\n' "$BLUE"   "$RESET" "$*"; }
ok()    { printf '%s✓%s  %s\n' "$GREEN"  "$RESET" "$*"; }
warn()  { printf '%s!%s  %s\n' "$YELLOW" "$RESET" "$*" >&2; }
fail()  { printf '%s✗%s  %s\n' "$RED"    "$RESET" "$*" >&2; exit 1; }
need_root() { [[ $EUID -eq 0 ]] || fail "must run as root (use sudo)"; }
have()  { command -v "$1" >/dev/null 2>&1; }

trap 'fail "aborted at line $LINENO"' ERR

# ── Detection ─────────────────────────────────────────────────────────────
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    armv7l)        echo "armv6l" ;;
    *) fail "unsupported architecture $(uname -m)" ;;
  esac
}

detect_pkg_mgr() {
  if   have apt-get; then echo "apt"
  elif have dnf;     then echo "dnf"
  elif have yum;     then echo "yum"
  else fail "no supported package manager (apt / dnf / yum)"
  fi
}

# ── Step 1: Go ────────────────────────────────────────────────────────────
# GO_VERSION is the MINIMUM the migrater needs (kept in lockstep with go.mod's
# `go` directive) and the version we install when the host is short of it. We
# only ever move Go FORWARD — a host that already has GO_VERSION or newer is
# used untouched, never downgraded.
GO_WE_INSTALLED=0      # 1 = THIS script laid Go down at GO_INSTALL_DIR
GO_OLD_VERSION=""      # pre-existing version string, for the log

# version_ge A B → success (0) iff version A >= version B (numeric-aware).
version_ge() { [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n1)" == "$1" ]]; }

ensure_go() {
  local arch tarball url cur
  arch=$(detect_arch)
  tarball="go${GO_VERSION}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"

  if have go; then
    cur="$(go version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -n1 || true)"
    GO_OLD_VERSION="${cur}"
    if [[ -n "${cur}" ]] && version_ge "${cur}" "${GO_VERSION}"; then
      ok "Go ${cur} already installed (>= ${GO_VERSION} required) — using it as-is"
      return
    fi
    info "host Go ${cur:-unknown} is older than ${GO_VERSION}; upgrading"
  else
    info "installing Go ${GO_VERSION} (fresh)"
  fi

  rm -rf "${GO_INSTALL_DIR}"
  curl -fsSL --retry 3 -o "/tmp/${tarball}" "${url}"
  tar -C "$(dirname "${GO_INSTALL_DIR}")" -xzf "/tmp/${tarball}"
  rm -f "/tmp/${tarball}"
  GO_WE_INSTALLED=1

  if ! grep -q "${GO_INSTALL_DIR}/bin" /etc/profile.d/go.sh 2>/dev/null; then
    cat > /etc/profile.d/go.sh <<EOF
export PATH=${GO_INSTALL_DIR}/bin:\$PATH:\$HOME/go/bin
EOF
    chmod +x /etc/profile.d/go.sh
  fi
  # Prepend so the toolchain we just unpacked wins over any older packaged `go`.
  export PATH="${GO_INSTALL_DIR}/bin:${PATH}:${HOME}/go/bin"
  ok "Go installed: $(go version)"
}

# Called at the end of main — removes ONLY a Go that this script installed.
# A pre-existing/adequate toolchain is left exactly as we found it.
cleanup_go() {
  if [[ "${GO_WE_INSTALLED}" -ne 1 ]]; then
    info "leaving host Go ${GO_OLD_VERSION:-} untouched (not installed by us)"
    return
  fi
  if [[ -n "${GO_OLD_VERSION}" ]]; then
    info "removing Go ${GO_VERSION} we installed (host had ${GO_OLD_VERSION} before — reinstall it if needed)"
  else
    info "removing Go ${GO_VERSION} (was not present before migration)"
  fi
  rm -rf "${GO_INSTALL_DIR}"
  rm -f /etc/profile.d/go.sh
  ok "Go removed"
}

# ── Step 0: Docker presence ───────────────────────────────────────────────
# Bail early on hosts that don't have Docker at all. v3 always installs
# Docker, so its absence means this isn't a v3 host — operator should install
# gocoolify directly instead of trying to "migrate" nothing.
require_docker() {
  have docker || fail "docker not installed — this script is for hosts running Coolify v3. For a fresh install, run gocoolify's install.sh directly."
  ok "docker present: $(docker --version 2>/dev/null || echo unknown)"
}

# ── Step 3: freeze v3 ─────────────────────────────────────────────────────
# Stop v3's management containers before any further step. Three reasons:
#   1. Releases TCP :3000 so coolifygo can bind it later.
#   2. Quiesces SQLite (`/app/db/prod.db`) so the upcoming `docker cp` reads a
#      consistent snapshot. docker cp itself works on stopped containers.
#   3. Lets us safely upgrade Docker without v3 trying to write mid-restart.
# Idempotent: if the containers are already stopped or missing, this is a no-op.
freeze_v3() {
  info "freezing v3 management plane"
  local stopped=0
  for name in coolify coolify-fluentbit; do
    if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$name"; then
      docker stop -t 30 "$name" >/dev/null && { ok "stopped $name"; stopped=$((stopped+1)); }
    fi
  done
  if [[ $stopped -eq 0 ]]; then
    ok "no running v3 management containers (already frozen, or v3 not installed)"
  fi
}

# ── Step 5: coolifygo install ─────────────────────────────────────────────
ensure_coolifygo() {
  # Probe by container: gocoolify install.sh names it "coolifygo".
  if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx coolifygo; then
    ok "coolifygo already running — skipping install"
    return
  fi
  info "installing coolifygo via ${COOLIFYGO_INSTALL_URL}"
  curl -fsSL "${COOLIFYGO_INSTALL_URL}" | bash
  # Wait briefly for coolifygo's Postgres + Redis to come up.
  local i=0
  while ! docker ps --format '{{.Names}}' | grep -qx coolifygo-postgres; do
    ((i++ >= 60)) && fail "coolifygo-postgres never appeared after 60s"
    sleep 1
  done
  ok "coolifygo installed"
}

# ── Step 2: migrater binary ───────────────────────────────────────────────
ensure_migrater() {
  if have coolfymigrater; then
    ok "coolfymigrater already on PATH: $(command -v coolfymigrater)"
    return
  fi
  info "go install ${MIGRATER_MODULE}@${MIGRATER_REF}"
  GOBIN=/usr/local/bin go install "${MIGRATER_MODULE}@${MIGRATER_REF}"
  have coolfymigrater || fail "go install completed but coolfymigrater not on PATH"
  ok "coolfymigrater installed: $(command -v coolfymigrater)"
}

# ── Step 6: env from coolifygo ────────────────────────────────────────────
load_coolifygo_env() {
  [[ -r "${COOLIFYGO_ENV}" ]] || fail "${COOLIFYGO_ENV} not readable — is coolifygo provisioned?"
  set -a
  # shellcheck source=/dev/null
  . "${COOLIFYGO_ENV}"
  set +a
  [[ -n "${DATABASE_URL:-}" ]]         || fail "DATABASE_URL not set after sourcing ${COOLIFYGO_ENV}"
  [[ -n "${DATA_ENCRYPTION_KEY:-}" ]]  || fail "DATA_ENCRYPTION_KEY not set after sourcing ${COOLIFYGO_ENV}"
  ok "loaded coolifygo env from ${COOLIFYGO_ENV}"
}

# ── Step 7: pre-docker phase ──────────────────────────────────────────────
run_pre_docker() {
  info "running coolfymigrater --phase=pre-docker"
  local yes_flag=""
  [[ "${ASSUME_YES}" == "1" ]] && yes_flag="--yes"
  coolfymigrater --phase=pre-docker --state-file="${STATE_FILE}" ${yes_flag}
  [[ -s "${STATE_FILE}" ]] || fail "pre-docker did not produce state file at ${STATE_FILE}"
  ok "data committed to coolifygo Postgres; state at ${STATE_FILE}"
}

# ── Step 4: Docker upgrade ────────────────────────────────────────────────
# Always-on per the design — v3 ships with an old engine. Runs after freeze_v3
# but before coolifygo install, so the daemon restart is safe: v3 management
# containers are stopped (unless-stopped policy keeps them down), workload
# containers bounce back via their restart policy, and coolifygo isn't up yet.
upgrade_docker() {
  info "upgrading Docker engine"
  local pm
  pm=$(detect_pkg_mgr)
  case "${pm}" in
    apt)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y --only-upgrade \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || \
      DEBIAN_FRONTEND=noninteractive apt-get install -y \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      ;;
    dnf|yum)
      "${pm}" -y upgrade docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || \
      "${pm}" -y install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      ;;
  esac

  # The package upgrade restarts dockerd, but we still wait so the API is
  # actually answering before we try to take over containers.
  info "waiting for Docker daemon to become responsive"
  local i=0
  until docker info >/dev/null 2>&1; do
    ((i++ >= 60)) && fail "docker daemon did not respond within 60s of upgrade"
    sleep 1
  done
  ok "Docker now: $(docker --version)"
}

# ── Step 8: post-docker phase ─────────────────────────────────────────────
run_post_docker() {
  info "running coolfymigrater --phase=post-docker"
  local yes_flag=""
  [[ "${ASSUME_YES}" == "1" ]] && yes_flag="--yes"
  coolfymigrater --phase=post-docker --state-file="${STATE_FILE}" ${yes_flag}
  ok "takeover + teardown finished"
}

# ── main ──────────────────────────────────────────────────────────────────
main() {
  need_root
  info "coolfymigrater install.sh — Coolify v3 → coolifygo on $(hostname)"
  require_docker
  ensure_go
  ensure_migrater
  freeze_v3
  upgrade_docker
  ensure_coolifygo
  load_coolifygo_env
  run_pre_docker
  run_post_docker
  cleanup_go
  ok "all done — Coolify v3 is gone, coolifygo owns the host"
}

main "$@"
