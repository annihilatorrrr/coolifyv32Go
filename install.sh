#!/usr/bin/env bash
# coolfymigrater — one-shot wrapper that turns a host running Coolify v3 into
# a host running coolifygo with every app + database migrated, then wipes v3.
#
# Operator usage:
#   curl -fsSL https://raw.githubusercontent.com/annihilatorrrr/coolifyv32Go/main/install.sh | sudo bash
#
# The wrapper:
#   1. Installs Go (if missing) at /usr/local/go using the official tarball.
#   2. Installs coolifygo via the gocoolify install.sh — skips if already up.
#   3. `go install`s the coolfymigrater binary from this repo's main branch.
#   4. Runs the binary `--phase=pre-docker` (discover → freeze → SQLite extract → insert).
#   5. Upgrades the host Docker engine (always-on; v3 ships with an old version).
#   6. Runs the binary `--phase=post-docker` (takeover → wipe v3).
#
# Idempotent on re-runs: each step probes for existing state before acting,
# so a failed run can be re-launched without compounding damage.

set -Eeuo pipefail

# ── Config ────────────────────────────────────────────────────────────────
GO_VERSION="${GO_VERSION:-1.26.3}"
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
ensure_go() {
  if have go && go version | grep -qE "go${GO_VERSION//./\\.}"; then
    ok "Go ${GO_VERSION} already installed: $(go version)"
    return
  fi
  info "installing Go ${GO_VERSION}"
  local arch tarball url
  arch=$(detect_arch)
  tarball="go${GO_VERSION}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"

  rm -rf "${GO_INSTALL_DIR}"
  curl -fsSL --retry 3 -o "/tmp/${tarball}" "${url}"
  tar -C "$(dirname "${GO_INSTALL_DIR}")" -xzf "/tmp/${tarball}"
  rm -f "/tmp/${tarball}"

  # Persist PATH for future shells; export for the current run.
  if ! grep -q "${GO_INSTALL_DIR}/bin" /etc/profile.d/go.sh 2>/dev/null; then
    cat > /etc/profile.d/go.sh <<EOF
export PATH=\$PATH:${GO_INSTALL_DIR}/bin:\$HOME/go/bin
EOF
    chmod +x /etc/profile.d/go.sh
  fi
  export PATH="${PATH}:${GO_INSTALL_DIR}/bin:${HOME}/go/bin"
  ok "Go installed: $(go version)"
}

# ── Step 2: coolifygo ─────────────────────────────────────────────────────
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

# ── Step 3: migrater binary ───────────────────────────────────────────────
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

# ── Step 4: env from coolifygo ────────────────────────────────────────────
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

# ── Step 5: pre-docker phase ──────────────────────────────────────────────
run_pre_docker() {
  info "running coolfymigrater --phase=pre-docker"
  local yes_flag=""
  [[ "${ASSUME_YES}" == "1" ]] && yes_flag="--yes"
  coolfymigrater --phase=pre-docker --state-file="${STATE_FILE}" ${yes_flag}
  [[ -s "${STATE_FILE}" ]] || fail "pre-docker did not produce state file at ${STATE_FILE}"
  ok "data committed to coolifygo Postgres; state at ${STATE_FILE}"
}

# ── Step 6: Docker upgrade ────────────────────────────────────────────────
# Always-on per the design — v3 ships with an old engine. Containers carrying
# unless-stopped restart policy come back automatically after daemon restart;
# the freeze in pre-docker left v3's management containers stopped, so they
# stay down. coolifygo-postgres just committed; it's safe to bounce.
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

# ── Step 7: post-docker phase ─────────────────────────────────────────────
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
  ensure_go
  ensure_coolifygo
  ensure_migrater
  load_coolifygo_env
  run_pre_docker
  upgrade_docker
  run_post_docker
  ok "all done — Coolify v3 is gone, coolifygo owns the host"
}

main "$@"
