#!/bin/bash
# hiclaw-env.sh - Worker-side environment bootstrap
#
# Lighter than the manager version (no base.sh dependency).
# Shares the same variable contract so scripts are portable.
#
# Provides:
#   HICLAW_RUNTIME         — "cloud-aliyun" | "docker"
#   CLOUD_SAE_MODE         — "true" | "false" (convenience flag)
#   HICLAW_MATRIX_SERVER   — Matrix server URL
#   HICLAW_STORAGE_BUCKET  — bucket name for mc commands
#   HICLAW_STORAGE_PREFIX  — "hiclaw/<bucket>" ready for mc paths
#   ensure_mc_credentials  — callable function (no-op in local mode)
#
# Usage:
#   source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ── Runtime detection ─────────────────────────────────────────────────────────
CLOUD_SAE_MODE=false
if [ -n "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-}" ] && \
   [ -f "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-/nonexistent}" ]; then
    CLOUD_SAE_MODE=true
    HICLAW_RUNTIME="cloud-aliyun"
else
    HICLAW_RUNTIME="docker"
fi

# ── Normalized variables ──────────────────────────────────────────────────────
HICLAW_MATRIX_SERVER="${HICLAW_MATRIX_URL:-http://127.0.0.1:6167}"
HICLAW_STORAGE_BUCKET="${HICLAW_OSS_BUCKET:-hiclaw-storage}"
HICLAW_STORAGE_PREFIX="hiclaw/${HICLAW_STORAGE_BUCKET}"

# ── Credential management ────────────────────────────────────────────────────
source /opt/hiclaw/scripts/lib/oss-credentials.sh 2>/dev/null || true

export HICLAW_RUNTIME CLOUD_SAE_MODE HICLAW_MATRIX_SERVER HICLAW_STORAGE_BUCKET HICLAW_STORAGE_PREFIX
