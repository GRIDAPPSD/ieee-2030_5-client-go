#!/usr/bin/env bash
# Run two simulated aggregators on operator-supplied identities, four managed
# devices each, for a hand test against one server. Both run in the
# foreground of this script; Ctrl-C stops both.
# Usage: start-two-aggregators.sh [start|unpair]
# Environment: SUNSPEC_DIR (required, outside the checkout, read only) holds
#   ca.crt            the trust bundle the clients verify the server with
#   agg-N.crt         aggregator N's full chain file, leaf first (N = 1, 2)
#   agg-N.key         aggregator N's private key
#   agg-N-dev-M.crt   managed device M of aggregator N (M = 1 to 4); only
#                     its LFDI is used, so a chain file is fine
# plus DERMS_DIR (generated state, default ~/derms-certs), SEP2_URL,
# ADMIN_URL and SEP2_ADMIN_KEY as in start-aggregator.sh. client-go checks the
# server certificate against ca.crt by chain only, so SEP2_URL may name an IP.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# start-aggregator.sh runs nothing when sourced; this reuses its admin API
# helpers so pairs are registered exactly as the single-aggregator script does.
# shellcheck source-path=SCRIPTDIR source=start-aggregator.sh
source "$SCRIPT_DIR/start-aggregator.sh"

AGGS=(1 2)
DEVS=(1 2 3 4)

# resolve_sunspec_dir canonicalizes SUNSPEC_DIR and refuses one inside the
# checkout, so supplied certificate material never sits next to the code.
resolve_sunspec_dir() {
  [[ -n "${SUNSPEC_DIR:-}" ]] || die "SUNSPEC_DIR is not set; point it at the directory holding the supplied certificates (see the header of $0)"
  [[ -d "$SUNSPEC_DIR" ]] || die "SUNSPEC_DIR $SUNSPEC_DIR is not a directory"
  SUNSPEC_DIR="$(cd "$SUNSPEC_DIR" && pwd -P)" || die "cannot enter SUNSPEC_DIR"
  case "$SUNSPEC_DIR/" in
    "$DERMS_REPO"/*) die "SUNSPEC_DIR $SUNSPEC_DIR is inside the checkout $DERMS_REPO; choose a directory outside it" ;;
  esac
}

sun_files() {
  local a n
  printf '%s\n' "$SUNSPEC_DIR/ca.crt"
  for a in "${AGGS[@]}"; do
    printf '%s\n' "$SUNSPEC_DIR/agg-$a.crt" "$SUNSPEC_DIR/agg-$a.key"
    for n in "${DEVS[@]}"; do printf '%s\n' "$SUNSPEC_DIR/agg-$a-dev-$n.crt"; done
  done
}

# check_distinct refuses a layout that gives one LFDI to two identities: the
# server holds one EndDevice per LFDI, and a repeat would pair a device under
# two managers or the aggregator under itself.
check_distinct() {
  local dups
  dups="$(printf '%s\n' "$@" | sort | uniq -d)"
  [[ -z "$dups" ]] || die "an LFDI is used more than once across the identities: $dups"
}

# write_agg_config N DEVLFDI... writes aggregator N's sim
# config. It holds paths and LFDIs only, no key material.
write_agg_config() {
  local a="$1"
  shift
  local rec="$DERMS_REPO/sim/derms/recordings" tmp lfdis_json
  tmp="$(mktemp "$DERMS_DIR/.agg-$a.json.XXXXXX")" || die "cannot create a temporary config file"
  lfdis_json="$(printf '%s\n' "$@" | jq -R . | jq -sc .)"
  jq -n \
    --arg server "${SEP2_URL:-https://127.0.0.1:8443}" \
    --arg cert "$SUNSPEC_DIR/agg-$a.crt" --arg key "$SUNSPEC_DIR/agg-$a.key" --arg ca "$SUNSPEC_DIR/ca.crt" \
    --arg name "agg-$a" --arg pvf "$rec/agg-pv-1.csv" --argjson lfdis "$lfdis_json" \
    '{role: "aggregator", server: $server, cert: $cert, key: $key, ca: $ca,
      device: {name: $name},
      managed: [range(0; ($lfdis | length)) as $i
        | {name: ($name + "-dev-" + (($i + 1) | tostring)), lfdi: $lfdis[$i],
           device: {name: ($name + "-dev-" + (($i + 1) | tostring)), type: "pv", rated_w: 4000},
           replay: {file: $pvf, clock: "wall"}}]}' >"$tmp" || die "cannot write the config of aggregator $a"
  mv -f "$tmp" "$DERMS_DIR/agg-$a.json"
}

declare -A AGG_LFDI
declare -A DEV_LFDI

# derive_lfdis fills AGG_LFDI[N] and DEV_LFDI["N-M"] from the certificates.
derive_lfdis() {
  local a n all=()
  for a in "${AGGS[@]}"; do
    AGG_LFDI[$a]="$(lfdi_of "$SUNSPEC_DIR/agg-$a.crt")"
    all+=("${AGG_LFDI[$a]}")
    for n in "${DEVS[@]}"; do
      DEV_LFDI["$a-$n"]="$(lfdi_of "$SUNSPEC_DIR/agg-$a-dev-$n.crt")"
      all+=("${DEV_LFDI["$a-$n"]}")
    done
  done
  check_distinct "${all[@]}"
}

two_checks() {
  require_admin_key
  require_safe_admin_url
  need_tool openssl
  need_tool sha256sum
  need_tool curl
  need_tool jq
  resolve_sunspec_dir
  local files=()
  mapfile -t files < <(sun_files)
  require_files "${files[@]}"
}

cmd_start() {
  two_checks
  derive_lfdis
  local a n devs
  for a in "${AGGS[@]}"; do
    devs=()
    for n in "${DEVS[@]}"; do devs+=("${DEV_LFDI["$a-$n"]}"); done
    write_agg_config "$a" "${devs[@]}"
  done
  for a in "${AGGS[@]}"; do
    for n in "${DEVS[@]}"; do pair_device "${AGG_LFDI[$a]}" "${DEV_LFDI["$a-$n"]}"; done
  done
  build_client
  rm -f "$ADMIN_BODY"
  local pids=() pid rc=0
  # Both clients are children of this shell, so one trap stops both; the
  # trap names pids this script started, never a pattern.
  trap 'kill "${pids[@]}" 2>/dev/null || true; exit 130' INT TERM
  for a in "${AGGS[@]}"; do
    # env -u keeps an exported SEP2_* value from overriding the config.
    env -u SEP2_SERVER -u SEP2_CERT -u SEP2_KEY -u SEP2_CA \
      "$DERMS_DIR/inverterclient" --sim-config "$DERMS_DIR/agg-$a.json" &
    pids+=("$!")
  done
  for pid in "${pids[@]}"; do
    wait "$pid" || rc=$?
  done
  exit "$rc"
}

cmd_unpair() {
  two_checks
  derive_lfdis
  local a n
  for a in "${AGGS[@]}"; do
    for n in "${DEVS[@]}"; do unpair_device "${DEV_LFDI["$a-$n"]}"; done
  done
}

main() {
  local sub="${1:-start}"
  case "$sub" in
    start | unpair) ;;
    *) die "usage: $0 [start|unpair]" ;;
  esac
  derms_dir_init
  # The response body of each admin call lands here; removed on every exit.
  ADMIN_BODY="$(mktemp "$DERMS_DIR/.admin-body.XXXXXX")" || die "cannot create a temporary file"
  trap 'rm -f "$ADMIN_BODY"' EXIT
  "cmd_$sub"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
