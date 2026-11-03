#!/usr/bin/env bash
# CI only: starts `phenix ui` on the runner, then creates and starts the
# scorch-demo experiment that live.spec.js captures (see ../README.md).
# Needs a minimega daemon on <base>/minimega, the ntp.qc2 image in its files
# directory, and /usr/local/bin/phenix built with the web UI embedded. Runs
# phenix as root, as minimega is.
#
# Usage: run-experiment.sh [start|stop]
#
# Environment: DOCS_LIVE_EXPERIMENT (scorch-demo), DOCS_LIVE_STORE
# (bolt:///etc/phenix/store.bdb), DOCS_LIVE_MM_BASE (/tmp/minimega),
# DOCS_LIVE_UI_ADDR (127.0.0.1:3080), DOCS_LIVE_LOG (/tmp/phenix-ui.log).

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
exp="${DOCS_LIVE_EXPERIMENT:-scorch-demo}"
store="${DOCS_LIVE_STORE:-bolt:///etc/phenix/store.bdb}"
mm_base="${DOCS_LIVE_MM_BASE:-/tmp/minimega}"
ui_addr="${DOCS_LIVE_UI_ADDR:-127.0.0.1:3080}"
log="${DOCS_LIVE_LOG:-/tmp/phenix-ui.log}"

phenix() {
  sudo /usr/local/bin/phenix --store.endpoint "$store" --base-dir.minimega "$mm_base" "$@"
}

start() {
  # One root shell starts the server detached from this step's shell, with
  # its output in the log; the arguments reach it as a vector, not as text.
  sudo bash -c 'out=$1; shift; setsid nohup "$@" >"$out" 2>&1 &' bash "$log" \
    /usr/local/bin/phenix --store.endpoint "$store" --base-dir.minimega "$mm_base" \
    ui --listen-endpoint "$ui_addr"
  for _ in $(seq 60); do
    if curl -sf --max-time 2 "http://$ui_addr/healthz" >/dev/null; then
      break
    fi
    sleep 1
  done
  if ! curl -sf --max-time 2 "http://$ui_addr/healthz" >/dev/null; then
    echo "::error::phenix ui did not answer on $ui_addr within 60 s" >&2
    sudo tail -50 "$log" >&2
    exit 1
  fi
  phenix version

  phenix experiment create "$exp" \
    --topology "$here/topology.yaml" --scenario "$here/scenario.yaml"

  # Start runs through the post-start stage, where the soh app waits for the
  # guests' C2 agents (about 90 s on a runner) and runs its checks.
  start=$(date +%s)
  timeout 25m sudo /usr/local/bin/phenix --store.endpoint "$store" \
    --base-dir.minimega "$mm_base" experiment start "$exp"
  echo "experiment start took $(( $(date +%s) - start )) s"

  phenix experiment list
  phenix vm info "$exp"
  echo "::group::state of health"
  curl -sf "http://$ui_addr/api/v1/experiments/$exp/soh" | python3 -m json.tool || true
  echo "::endgroup::"
}

stop() {
  # Best effort: a run left at a break point, or a failed start, must not
  # keep the runner's teardown from finishing.
  curl -sf -X DELETE "http://$ui_addr/api/v1/experiments/$exp/scorch/pipelines/0" || true
  curl -sf -X DELETE "http://$ui_addr/api/v1/experiments/$exp/scorch/pipelines/1" || true
  phenix experiment stop "$exp" || true
  phenix experiment delete "$exp" || true
  sudo pkill -INT -x phenix || true
}

case "${1:-start}" in
  start) start ;;
  stop) stop ;;
  *)
    echo "usage: $0 [start|stop]" >&2
    exit 2
    ;;
esac
