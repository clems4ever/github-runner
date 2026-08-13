#!/usr/bin/env bash
set -euo pipefail

# Registers a self-hosted GitHub Actions runner from a registration token and
# runs it. The registration is saved so restarts do not need a new token.
#
# Required:
#   GITHUB_URL       URL of the repository or organisation the runner joins,
#                    e.g. https://github.com/clems4ever/runyard
#   RUNNER_TOKEN     Registration token copied from
#                    Settings > Actions > Runners > New self-hosted runner.
#                    Only needed to register: the token expires an hour after
#                    it was issued, but the runner then talks to GitHub with
#                    credentials of its own. Once a registration has been saved
#                    to RUNNER_STATE_DIR the token can be dropped from .env.
#
# Optional:
#   RUNNER_NAME      Defaults to the container hostname.
#   RUNNER_LABELS    Comma-separated extra labels, e.g. "docker,self-hosted".
#   RUNNER_GROUP     Runner group, defaults to "Default".
#   RUNNER_WORKDIR   Work directory, defaults to /home/runner/_work.
#   RUNNER_STATE_DIR Where the registration is saved, defaults to
#                    /home/runner/.runner-state. Mount a volume there to keep
#                    it across container re-creation.
#   EPHEMERAL        "true" to accept a single job then exit. Needs a valid
#                    RUNNER_TOKEN for every restart, see below.
#   DISABLE_UPDATE   "true" to prevent the runner from auto-updating itself.

RUNNER_DIR=/home/runner/actions-runner
RUNNER_NAME=${RUNNER_NAME:-$(hostname)}
RUNNER_GROUP=${RUNNER_GROUP:-Default}
RUNNER_WORKDIR=${RUNNER_WORKDIR:-/home/runner/_work}
EPHEMERAL=${EPHEMERAL:-false}
# One directory per runner name, so replicas sharing the state volume cannot
# pick up each other's registration.
STATE_DIR=${RUNNER_STATE_DIR:-/home/runner/.runner-state}/${RUNNER_NAME}
# Everything config.sh writes, and all the runner needs to reconnect later.
CONFIG_FILES=(.runner .credentials .credentials_rsakey)

log() { echo "[entrypoint] $*"; }
die() { echo "[entrypoint] error: $*" >&2; exit 1; }

[[ -n "${GITHUB_URL:-}" ]] || die "GITHUB_URL is required"

cd "$RUNNER_DIR"

# An ephemeral runner is dropped by GitHub after its job, so every container
# start has to register again — which only works while the token is valid.
saved_registration_matches() {
  [[ "$EPHEMERAL" == "true" ]] && return 1
  [[ -f "$STATE_DIR/.runner" && -f "$STATE_DIR/.credentials" ]] || return 1

  local url
  url=$(jq -r '.gitHubUrl // .serverUrl // empty' "$STATE_DIR/.runner")
  if [[ "${url%/}" != "${GITHUB_URL%/}" ]]; then
    log "saved registration points at '${url}', registering again"
    return 1
  fi
}

restore_registration() {
  local file
  for file in "${CONFIG_FILES[@]}"; do
    if [[ -f "$STATE_DIR/$file" ]]; then cp -p "$STATE_DIR/$file" "$RUNNER_DIR/$file"; fi
  done
  log "reusing the saved registration of '${RUNNER_NAME}' on ${GITHUB_URL}"
}

save_registration() {
  if [[ "$EPHEMERAL" == "true" ]]; then return 0; fi
  local file
  mkdir -p "$STATE_DIR"
  for file in "${CONFIG_FILES[@]}"; do
    if [[ -f "$RUNNER_DIR/$file" ]]; then cp -p "$RUNNER_DIR/$file" "$STATE_DIR/$file"; fi
  done
  log "saved the registration to ${STATE_DIR}, RUNNER_TOKEN is no longer needed"
}

register() {
  [[ -n "${RUNNER_TOKEN:-}" ]] || die "RUNNER_TOKEN is required: copy a fresh registration token from ${GITHUB_URL}/settings/actions/runners/new"

  # config.sh refuses to configure on top of an existing registration, which a
  # restarted container still has on disk.
  if [[ -f "$RUNNER_DIR/.runner" ]]; then
    log "discarding the stale local registration"
    ./config.sh remove --local || rm -f "${CONFIG_FILES[@]}"
  fi

  local config_args=(
    --url "$GITHUB_URL"
    --name "$RUNNER_NAME"
    --runnergroup "$RUNNER_GROUP"
    --work "$RUNNER_WORKDIR"
    --unattended
    # Take over the entry of a runner of the same name left behind by a
    # previous container; nothing de-registers it, as that needs a token too.
    --replace
  )
  if [[ -n "${RUNNER_LABELS:-}" ]]; then config_args+=(--labels "$RUNNER_LABELS"); fi
  if [[ "$EPHEMERAL" == "true" ]]; then config_args+=(--ephemeral); fi
  if [[ "${DISABLE_UPDATE:-false}" == "true" ]]; then config_args+=(--disableupdate); fi

  log "registering runner '${RUNNER_NAME}' on ${GITHUB_URL}"
  ./config.sh "${config_args[@]}" --token "$RUNNER_TOKEN" \
    || die "registration failed (a registration token expires one hour after it is issued)"
  save_registration
}

if saved_registration_matches; then
  restore_registration
else
  if [[ "$EPHEMERAL" == "true" ]]; then
    log "warning: EPHEMERAL=true registers again after every job, so the runner stops coming back once RUNNER_TOKEN expires"
  fi
  register
fi

terminate() {
  log "shutdown signal received, stopping runner"
  kill -TERM "$runner_pid" 2>/dev/null || true
  wait "$runner_pid" 2>/dev/null || true
  exit 0
}
trap terminate INT TERM

log "starting runner"
./run.sh "$@" &
runner_pid=$!
# `wait` is interrupted by the trapped signals, which is what lets `terminate`
# run before the container is killed.
exit_code=0
wait "$runner_pid" || exit_code=$?
exit "$exit_code"
