#!/usr/bin/env bash
set -euo pipefail

# Registers a self-hosted GitHub Actions runner, runs it, and de-registers it
# again on shutdown.
#
# Required:
#   GITHUB_URL       URL of the repository or organisation the runner joins,
#                    e.g. https://github.com/clems4ever/runyard
#   ACCESS_TOKEN     GitHub PAT used to mint a registration token
#                    (classic: "repo" scope for a repo runner, "admin:org" for
#                    an org runner; fine-grained: "Administration: read/write").
#                    Alternatively set RUNNER_TOKEN to a registration token
#                    obtained from the Settings > Actions > Runners page — but
#                    those expire after one hour.
#
# Optional:
#   RUNNER_NAME      Defaults to the container hostname.
#   RUNNER_LABELS    Comma-separated extra labels, e.g. "docker,self-hosted".
#   RUNNER_GROUP     Runner group, defaults to "Default".
#   RUNNER_WORKDIR   Work directory, defaults to /home/runner/_work.
#   EPHEMERAL        "true" to accept a single job then exit (recommended).
#   DISABLE_UPDATE   "true" to prevent the runner from auto-updating itself.
#   GITHUB_API_URL   Defaults to https://api.github.com (override for GHES).

RUNNER_DIR=/home/runner/actions-runner
GITHUB_API_URL=${GITHUB_API_URL:-https://api.github.com}
RUNNER_NAME=${RUNNER_NAME:-$(hostname)}
RUNNER_GROUP=${RUNNER_GROUP:-Default}
RUNNER_WORKDIR=${RUNNER_WORKDIR:-/home/runner/_work}

log() { echo "[entrypoint] $*"; }
die() { echo "[entrypoint] error: $*" >&2; exit 1; }

[[ -n "${GITHUB_URL:-}" ]] || die "GITHUB_URL is required"
if [[ -z "${ACCESS_TOKEN:-}" && -z "${RUNNER_TOKEN:-}" ]]; then
  die "either ACCESS_TOKEN (a PAT) or RUNNER_TOKEN (a registration token) is required"
fi

# Turn https://github.com/owner[/repo] into the matching API path.
scope_path=${GITHUB_URL#*://}
scope_path=${scope_path#*/}
scope_path=${scope_path%/}
case "$scope_path" in
  enterprises/*) api_scope="${scope_path}" ;;
  */*)           api_scope="repos/${scope_path}" ;;
  ?*)            api_scope="orgs/${scope_path}" ;;
  *)             die "cannot derive owner/repo from GITHUB_URL=${GITHUB_URL}" ;;
esac

# action is either "registration" or "remove"
mint_token() {
  local action=$1 response token
  response=$(curl -fsSL -X POST \
    -H "Accept: application/vnd.github+json" \
    -H "Authorization: Bearer ${ACCESS_TOKEN}" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "${GITHUB_API_URL}/${api_scope}/actions/runners/${action}-token") \
    || die "failed to request a ${action} token for ${api_scope} (check ACCESS_TOKEN scopes)"
  token=$(jq -r '.token // empty' <<<"$response")
  [[ -n "$token" ]] || die "no token in the ${action}-token response"
  printf '%s' "$token"
}

cd "$RUNNER_DIR"

config_args=(
  --url "$GITHUB_URL"
  --name "$RUNNER_NAME"
  --runnergroup "$RUNNER_GROUP"
  --work "$RUNNER_WORKDIR"
  --unattended
  --replace
)
if [[ -n "${RUNNER_LABELS:-}" ]]; then config_args+=(--labels "$RUNNER_LABELS"); fi
if [[ "${EPHEMERAL:-false}" == "true" ]]; then config_args+=(--ephemeral); fi
if [[ "${DISABLE_UPDATE:-false}" == "true" ]]; then config_args+=(--disableupdate); fi

if [[ -n "${ACCESS_TOKEN:-}" ]]; then
  registration_token=$(mint_token registration)
else
  registration_token=$RUNNER_TOKEN
fi

log "registering runner '${RUNNER_NAME}' on ${GITHUB_URL}"
./config.sh "${config_args[@]}" --token "$registration_token"

deregister() {
  log "removing runner '${RUNNER_NAME}'"
  local remove_token
  if [[ -n "${ACCESS_TOKEN:-}" ]]; then
    remove_token=$(mint_token remove)
  else
    remove_token=$RUNNER_TOKEN
  fi
  ./config.sh remove --token "$remove_token" || log "de-registration failed, ignoring"
}
terminate() {
  log "shutdown signal received, stopping runner"
  kill -TERM "$runner_pid" 2>/dev/null || true
  wait "$runner_pid" 2>/dev/null || true
  deregister
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

# The runner exited on its own: an ephemeral runner that picked up its job, or
# a crash. Either way this registration is dead, so clean it up.
deregister
exit "$exit_code"
