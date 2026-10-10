#!/usr/bin/env bash
# Rein-only CI entry point; never borrows another repository's credentials.
set -euo pipefail
if [ -z "${ARK_TOKEN:-}" ]; then
  echo '::warning::ARK_TOKEN is absent; no landing recorded.'
  exit 0
fi
root="$(cd "$(dirname "$0")" && pwd)"
work="${RUNNER_TEMP:?}/rein-ark-landing"
mkdir -p "$work/bin" "$work/replica"
ARK_BIN_DIR="$work/bin" bash "$root/install-ark.sh"
export PATH="$work/bin:$PATH"
# A disposable CI replica joins the existing ID; never initialize the checkout.
cd "$work/replica"
git init -q -b main
git config user.name ark-record-landing
git config user.email ci-writer@elk.work
git -c commit.gpgsign=false commit -q --allow-empty -m 'landing scratch'
ark init --repository 01M17TTM53XJKBZW2M9E04HHYP >/dev/null
ark remote set https://ark-709757975936.us-west1.run.app >/dev/null
if [ -n "${PR:-}" ]; then
  exec python3 "$root/ark_record_landing.py" "$GITHUB_REPOSITORY" "$PR" "$SHA" --ark-dir "$work/replica"
fi
exec python3 "$root/ark_record_landing.py" "$GITHUB_REPOSITORY" --since-hours "${HOURS:-48}" --ark-dir "$work/replica"
