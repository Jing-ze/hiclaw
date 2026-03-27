#!/bin/bash
# hiclaw-env.sh - Unified environment bootstrap for HiClaw scripts
#
# Single source of truth for both Manager and Worker containers.
# Source this file instead of manually setting up Matrix/storage variables.
#
# Provides:
#   HICLAW_RUNTIME         — "aliyun" | "docker" | "none"
#   HICLAW_MATRIX_SERVER   — Matrix server URL (works in both local and cloud)
#   HICLAW_STORAGE_BUCKET  — bucket name for mc commands
#   HICLAW_STORAGE_PREFIX  — "hiclaw/<bucket>" ready for mc paths
#   ensure_mc_credentials  — callable function (no-op in local mode)
#
# Usage:
#   source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ── Optional dependencies ─────────────────────────────────────────────────────
# base.sh provides log(), waitForService(), generateKey() — Manager-only.
# Worker images don't ship base.sh; the silent fallback is intentional.
source /opt/hiclaw/scripts/lib/base.sh 2>/dev/null || true

# ── Runtime detection ─────────────────────────────────────────────────────────
# Respect pre-set HICLAW_RUNTIME (e.g. from Dockerfile.aliyun ENV), only detect if unset
if [ -z "${HICLAW_RUNTIME:-}" ]; then
    if [ -n "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-}" ] && \
       [ -f "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-/nonexistent}" ]; then
        HICLAW_RUNTIME="aliyun"
    elif [ -S "${HICLAW_CONTAINER_SOCKET:-/var/run/docker.sock}" ]; then
        HICLAW_RUNTIME="docker"
    else
        HICLAW_RUNTIME="none"
    fi
fi

# ── Normalized variables ──────────────────────────────────────────────────────
# Matrix server: cloud mode uses external NLB address, local uses localhost
HICLAW_MATRIX_SERVER="${HICLAW_MATRIX_URL:-http://127.0.0.1:6167}"

# AI Gateway: cloud mode uses env endpoint (HICLAW_AI_GATEWAY_URL), local uses domain:8080
HICLAW_AI_GATEWAY_SERVER="${HICLAW_AI_GATEWAY_URL:-http://${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}:8080}"

# Storage: cloud mode uses OSS bucket name, local uses MinIO default
HICLAW_STORAGE_BUCKET="${HICLAW_OSS_BUCKET:-hiclaw-storage}"
HICLAW_STORAGE_PREFIX="hiclaw/${HICLAW_STORAGE_BUCKET}"

# ── Credential management ────────────────────────────────────────────────────
# In cloud mode, provides ensure_mc_credentials() for STS token refresh.
# In local mode, ensure_mc_credentials() is a no-op.
source /opt/hiclaw/scripts/lib/oss-credentials.sh 2>/dev/null || true

export HICLAW_RUNTIME HICLAW_MATRIX_SERVER HICLAW_AI_GATEWAY_SERVER HICLAW_STORAGE_BUCKET HICLAW_STORAGE_PREFIX

# ── Sync exclude patterns ────────────────────────────────────────────────────
# Runtime/ephemeral directories that must NOT be synced to remote storage.
# mc binary is installed as mc.bin, so its config dir is .mc.bin/ (not .mc/).
HICLAW_SYNC_EXCLUDES=(
    ".mc.bin/**"
    ".mc/**"
    ".openclaw/**"
    ".copaw/**"
    ".copaw.secret/**"
    ".cache/**"
    ".npm/**"
    ".local/**"
    ".agents/**"
    "__pycache__/**"
    "credentials/**"
    "node_modules/**"
    "*.lock"
)

# hiclaw_mc_exclude_args [extra_patterns...]
# Populates HICLAW_MC_EXCLUDE_ARGS array with --exclude flags for mc mirror.
# Must be called before mc mirror; use "${HICLAW_MC_EXCLUDE_ARGS[@]}" to expand.
# Usage:
#   hiclaw_mc_exclude_args "openclaw.json" "config/mcporter.json"
#   mc mirror src/ dst/ --overwrite "${HICLAW_MC_EXCLUDE_ARGS[@]}"
hiclaw_mc_exclude_args() {
    HICLAW_MC_EXCLUDE_ARGS=()
    for pattern in "${HICLAW_SYNC_EXCLUDES[@]}"; do
        HICLAW_MC_EXCLUDE_ARGS+=(--exclude "$pattern")
    done
    for pattern in "$@"; do
        HICLAW_MC_EXCLUDE_ARGS+=(--exclude "$pattern")
    done
}
