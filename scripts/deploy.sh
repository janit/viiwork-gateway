#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# ── Deploy viiwork-gateway from a git tag ───────────────────────────────────
#
# Runs ON the deployment host (a.256.fi, as root, from /root/viiwork-gateway),
# not from a workstation. That checkout is a clone of the PUBLIC repo, so a
# release has to be published before it can be deployed — see scripts/
# publish.sh. Mirrors the shape of routemap4's scripts/deploy.sh: a lock, a
# tag, a build, and a health check that has to pass before the script claims
# success.
#
# Usage:
#   ./scripts/deploy.sh v0.2.0        deploy that tag
#   ./scripts/deploy.sh --latest      deploy the newest tag
#   ./scripts/deploy.sh --redeploy    rebuild whatever is checked out
#   ./scripts/deploy.sh v0.1.0        roll back (a tag is a tag)
#
# Full context, including the Caddy and Cloudflare setup this assumes is
# already done, is in docs/security/DEPLOYMENT.md.

LOCK_FILE="/tmp/viiwork-gateway-deploy.lock"
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "ERROR: another deploy is already running" >&2
  exit 1
fi

CONTAINER="viiwork-gateway"
# The host port from docker-compose.yaml. Loopback only — Caddy is the sole
# way in, so this is also the only address a health check can use.
PORT=8090
HEALTH_TIMEOUT=60

red()   { printf '\033[1;31m%s\033[0m\n' "$*"; }
green() { printf '\033[1;32m%s\033[0m\n' "$*"; }
info()  { printf '\033[1;34m→ %s\033[0m\n' "$*"; }
warn()  { printf '\033[1;33m! %s\033[0m\n' "$*"; }

TARGET=""; REDEPLOY=false; LATEST=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --latest)    LATEST=true; shift ;;
    --redeploy)  REDEPLOY=true; shift ;;
    -h|--help)   sed -n '6,22p' "$0" | sed 's/^# \?//'; exit 0 ;;
    -*)          red "Unknown flag: $1"; exit 1 ;;
    *)           TARGET="$1"; shift ;;
  esac
done

# ── Pre-flight ──────────────────────────────────────────────────────────────

# .env is not in the repo and never will be: it holds the real API keys. It is
# also the one thing a checkout cannot restore, so check for it before doing
# anything that stops the running container.
if [[ ! -f .env ]]; then
  red "No .env in $PWD — refusing to deploy."
  echo "  The gateway will not start without keys. Copy the bundle's .env here,"
  echo "  chmod 600 it, and re-run."
  exit 1
fi

# A dirty tree makes `git checkout <tag>` fail with a message about the tag,
# which sends you looking in the wrong place. Say what is actually wrong.
if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
  red "Working tree has local modifications — refusing to deploy."
  git status --short --untracked-files=no | sed 's/^/    /'
  echo
  echo "  This checkout should be a clean clone of the public repo. Commit,"
  echo "  stash or discard, then re-run."
  exit 1
fi

PREVIOUS=$(git describe --tags --exact-match 2>/dev/null || git rev-parse --short HEAD)

if ! $REDEPLOY; then
  info "Fetching tags"
  git fetch --tags --quiet

  if $LATEST; then
    TARGET=$(git tag -l --sort=-version:refname | head -1)
    [[ -z "$TARGET" ]] && { red "No tags found"; exit 1; }
  fi

  if [[ -z "$TARGET" ]]; then
    red "No tag given."
    echo "  Usage: $0 <tag> | --latest | --redeploy"
    echo
    echo "  Recent tags:"
    git tag -l --sort=-version:refname | head -5 | sed 's/^/    /'
    exit 1
  fi

  if ! git rev-parse -q --verify "refs/tags/$TARGET" >/dev/null; then
    red "No such tag: $TARGET"
    echo "  Recent tags:"
    git tag -l --sort=-version:refname | head -5 | sed 's/^/    /'
    exit 1
  fi

  info "Checking out $TARGET (was $PREVIOUS)"
  git checkout --quiet "$TARGET"
else
  TARGET="$PREVIOUS"
  info "Rebuilding $TARGET in place (--redeploy)"
fi

# ── Build and swap ──────────────────────────────────────────────────────────
#
# `up -d --build` builds first and only then recreates the container, so the
# outage is a container restart rather than a whole image build. It is still an
# outage: any in-flight completion or SSE stream is cut.
#
# The gateway is a mesh member now, so a restart is visible to the fleet too.
# Compose stops the container with SIGTERM, which drains HTTP and then leaves
# the mesh, so every node sees it go at once rather than declaring it dead a
# failure-detector interval later — and sees it rejoin seconds afterwards. No
# node depends on the gateway (nothing polls it), so this is still not worth a
# blue/green dance — but do not run it mid-generation and be surprised.

#
# VERSION is what the Dockerfile stamps into the binary with -X main.version.
# It is not cosmetic: the gateway gossips its version in its mesh metadata, so
# an unstamped build calls itself "dev" on every node's member list as well as
# in its own startup log.
info "Building and restarting"
VERSION="$TARGET" docker compose up -d --build

# ── Health ──────────────────────────────────────────────────────────────────
#
# Two checks, because they prove different things and the weaker one is what
# tells you the container is even alive.
#
# NOTE: there is no unauthenticated health endpoint. Every path requires a key,
# /health included — that is deliberate (see cmd/viiwork-gateway's
# TestEveryPathRequiresAKey), so a plain curl returning 401 is a PASS for
# liveness: the process is up, listening, and enforcing auth.
#
# Do NOT health-check "/" or "/mesh" here. The session cookie is Secure, so
# over plain-HTTP loopback a browser drops it and you get redirect loops or
# repeated 401s that look like a broken gateway. Those pages are verified
# through https://huono.trippi.app, not from the host.

info "Waiting for the gateway to answer on 127.0.0.1:$PORT"
healthy=false
for _ in $(seq 1 "$HEALTH_TIMEOUT"); do
  # Match a real HTTP status rather than testing against "000". curl -w
  # PRINTS 000 on a connection failure *and* exits non-zero, so the obvious
  # `|| echo 000` appends a second one and yields "000000" — which is not
  # equal to "000" and would report a dead gateway as healthy. Ask what we
  # actually mean instead: did something answer with a status line.
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/v1/models" 2>/dev/null || true)
  if [[ "$code" =~ ^[1-5][0-9][0-9]$ ]]; then
    healthy=true
    break
  fi
  sleep 1
done

if ! $healthy; then
  red "Gateway did not answer within ${HEALTH_TIMEOUT}s"
  echo
  docker compose logs --tail=40
  echo
  if [[ "$PREVIOUS" != "$TARGET" ]]; then
    warn "Still on $TARGET. To go back: ./scripts/deploy.sh $PREVIOUS"
  fi
  exit 1
fi

# The authenticated check. Reads the first VIIWORK_KEY_* out of .env rather
# than naming one: key labels are per-client and get rotated, and a health
# check that breaks when a key is renamed is worse than no health check.
KEY=$(sed -n 's/^VIIWORK_KEY_[A-Za-z0-9_]*=//p' .env | head -1)
if [[ -z "$KEY" ]]; then
  warn "Gateway is up (HTTP $code) but no VIIWORK_KEY_* found in .env to verify the mesh with."
  exit 0
fi

MODELS=$(curl -sS "http://127.0.0.1:${PORT}/v1/models" -H "Authorization: Bearer $KEY" 2>/dev/null || true)
# `|| true` is load-bearing: this script runs under `set -o pipefail`, and
# grep exits 1 when a healthy-but-empty mesh returns no models — which would
# kill the script here, mid-success, with no output at all.
# grep -o | wc -l, not grep -c: /v1/models is a single line of JSON, and
# grep -c counts matching LINES, so it would report "1 model" for any
# non-empty mesh. `|| true` guards the pipeline against set -o pipefail —
# grep exits 1 on an empty mesh, which would otherwise kill the script here
# mid-success with no output at all.
COUNT=$(printf '%s' "$MODELS" | grep -o '"id"' | wc -l || true)
COUNT=$(printf '%s' "$COUNT" | tr -d ' ')

if [[ "$COUNT" -gt 0 ]]; then
  green "Deployed $TARGET — gateway up, $COUNT model(s) visible across the mesh"
else
  # Up but seeing nothing is a real state worth distinguishing: the gateway is
  # fine and the fleet is not, so redeploying it will not help.
  warn "Deployed $TARGET — gateway is up, but /v1/models returned no models."
  echo "  That points at membership, not at the gateway. In order of likelihood:"
  echo "    - it has only just joined and no capacity report has landed yet;"
  echo "      re-run the check in a few seconds before believing this"
  echo "    - the mesh secret does not match the nodes (a secured gateway"
  echo "      cannot join an open mesh, or vice versa)"
  echo "    - gossip cannot reach the fleet: 7946 tcp+udp both ways"
  echo "    - VIIWORK_GW_VIEW_NODES names no node that is currently alive"
  echo "    - every node is genuinely down"
  echo "  'docker compose logs' shows which members it can see."
fi

# Only worth saying when there is somewhere to go back TO: on --redeploy the
# previous and current versions are the same, and "roll back to the thing you
# are already running" is advice that wastes someone's time in an incident.
if [[ "$PREVIOUS" != "$TARGET" ]]; then
  echo "  Previous: $PREVIOUS   (roll back with: ./scripts/deploy.sh $PREVIOUS)"
fi
