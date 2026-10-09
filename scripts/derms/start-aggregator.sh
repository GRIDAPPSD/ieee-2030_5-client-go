#!/usr/bin/env bash
# Run the simulated aggregator for the manual DERMS test, and drive it.
# Usage: start-aggregator.sh [start]
#        start-aggregator.sh reserve [--energy-wh N] [--power-w N] [--start-in-s N] [--duration-s N]
#        start-aggregator.sh withdraw
#        start-aggregator.sh unpair
# Environment: DERMS_DIR (default ~/derms-certs), SEP2_URL
# (default https://127.0.0.1:8443), ADMIN_URL (default http://127.0.0.1:8444;
# plain http is accepted only on loopback), SEP2_ADMIN_KEY (the admin key,
# required by start and unpair; `make run` in the server checkout sets it to
# "admin"), PICKUP_WAIT_S (default 10).
# reserve --energy-wh: positive charges the battery, negative discharges it,
# zero is refused (the client's convention).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$SCRIPT_DIR/lib.sh"

AGG=agg-1
BAT=agg-bat-1
PV=agg-pv-1

# require_admin_key refuses to go on without the admin key, before any
# request: the server needs a real credential even on loopback for the CA
# fetch and the pair writes. The key goes into a quoted curl config line, so
# a character that would break out of the quotes is refused too.
require_admin_key() {
  local key="${SEP2_ADMIN_KEY:-}"
  [[ -n "$key" ]] ||
    die "SEP2_ADMIN_KEY is not set; export the server's admin key (\`make run\` in the server checkout uses SEP2_ADMIN_KEY=admin)"
  case "$key" in
    *'"'* | *\\* | *$'\n'* | *$'\r'*)
      die "SEP2_ADMIN_KEY contains a double quote, backslash or newline, which cannot be passed to curl safely"
      ;;
  esac
}

# require_safe_admin_url refuses a plain-http ADMIN_URL off loopback: the
# Bearer key and the CA the client then trusts would cross the network in
# clear. The host must be followed by a port, a path or the end, so a name
# that merely starts with 127.0.0.1 does not pass.
require_safe_admin_url() {
  local url="${ADMIN_URL:-http://127.0.0.1:8444}"
  case "$url" in
    https://*) ;;
    http://127.0.0.1 | http://127.0.0.1[:/]* | http://localhost | http://localhost[:/]* | 'http://[::1]' | 'http://[::1]'[:/]*) ;;
    *) die "ADMIN_URL $url must be https, or http on 127.0.0.1, localhost or [::1]" ;;
  esac
}

# admin_curl METHOD PATH [BODY] prints the HTTP status and leaves the
# response body in ADMIN_BODY. The key goes to curl on stdin as a config
# line, so it never appears in a process list or in this script's output.
admin_curl() {
  local method="$1" path="$2" body="${3:-}"
  local url="${ADMIN_URL:-http://127.0.0.1:8444}$path"
  local args=(-sS --connect-timeout 5 --max-time 30 -o "$ADMIN_BODY" -w '%{http_code}' -X "$method")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi
  printf 'header = "Authorization: Bearer %s"\n' "$SEP2_ADMIN_KEY" | curl "${args[@]}" -K - "$url"
}

# fetch_ca writes the running server's CA to DERMS_DIR/ca.crt from
# GET /api/certs/ca, so the CA always matches the server being tested.
fetch_ca() {
  local code
  code="$(admin_curl GET /api/certs/ca)" || die "cannot reach the admin API at ${ADMIN_URL:-http://127.0.0.1:8444}"
  [[ "$code" == 200 ]] || die "GET /api/certs/ca answered $code"
  local pem
  pem="$(jq -er '.certPEM' "$ADMIN_BODY")" || die "GET /api/certs/ca returned no certPEM"
  [[ "$pem" == *"BEGIN CERTIFICATE"* ]] || die "certPEM is not a PEM certificate"
  # mktemp creates a fresh file, so a symlink planted at a fixed name is
  # never written through.
  local tmp
  tmp="$(mktemp "$DERMS_DIR/.ca.crt.XXXXXX")" || die "cannot create a temporary CA file"
  printf '%s\n' "$pem" >"$tmp"
  mv -f "$tmp" "$DERMS_DIR/ca.crt"
}

# write_config writes the aggregator's sim config, with the managed LFDIs
# filled in. Paths are absolute so the config does not depend on where it
# sits. It holds no secret.
write_config() {
  local bat_lfdi="$1" pv_lfdi="$2" rec="$DERMS_REPO/sim/derms/recordings" tmp
  tmp="$(mktemp "$DERMS_DIR/.$AGG.json.XXXXXX")" || die "cannot create a temporary config file"
  jq -n \
    --arg server "${SEP2_URL:-https://127.0.0.1:8443}" \
    --arg cert "$DERMS_DIR/$AGG.crt" --arg key "$DERMS_DIR/$AGG.key" --arg ca "$DERMS_DIR/ca.crt" \
    --arg req "$DERMS_DIR/frq-request.json" \
    --arg bat "$BAT" --arg batl "$bat_lfdi" --arg batf "$rec/agg-bat-1.csv" \
    --arg pv "$PV" --arg pvl "$pv_lfdi" --arg pvf "$rec/agg-pv-1.csv" \
    '{role: "aggregator", server: $server, cert: $cert, key: $key, ca: $ca,
      device: {name: "agg-1"},
      frq: {request_file: $req},
      managed: [
        {name: $bat, lfdi: $batl, device: {name: $bat, type: "battery", rated_w: 5000, capacity_wh: 10000},
         replay: {file: $batf, clock: "wall"}},
        {name: $pv, lfdi: $pvl, device: {name: $pv, type: "pv", rated_w: 4000},
         replay: {file: $pvf, clock: "wall"}}
      ]}' >"$tmp" || die "cannot write the aggregator config"
  mv -f "$tmp" "$DERMS_DIR/$AGG.json"
}

# pair_device creates one management pair. The server answers 201 on a
# repeat, so rerunning start is safe; 409 means another manager holds the
# device.
pair_device() {
  local manager="$1" managed="$2" body code
  body="$(jq -nc --arg m "$manager" --arg d "$managed" '{managerLFDI: $m, managedLFDI: $d}')"
  code="$(admin_curl POST /api/management-pairs "$body")" || die "cannot reach the admin API"
  case "$code" in
    201) echo "paired $managed under $manager" ;;
    409) die "device $managed is already managed by another manager (409)" ;;
    *) die "POST /api/management-pairs answered $code for $managed" ;;
  esac
}

unpair_device() {
  local managed="$1" code
  code="$(admin_curl DELETE "/api/management-pairs?managed=$managed")" || die "cannot reach the admin API"
  case "$code" in
    204) echo "unpaired $managed" ;;
    404) echo "pair for $managed already gone" ;;
    *) die "DELETE /api/management-pairs answered $code for $managed" ;;
  esac
}

cmd_start() {
  require_admin_key
  require_safe_admin_url
  need_tool openssl
  need_tool sha256sum
  need_tool curl
  need_tool jq
  require_files "$DERMS_DIR/$AGG.crt" "$DERMS_DIR/$AGG.key" "$DERMS_DIR/$BAT.crt" "$DERMS_DIR/$PV.crt"
  local agg_lfdi bat_lfdi pv_lfdi
  agg_lfdi="$(lfdi_of "$DERMS_DIR/$AGG.crt")"
  bat_lfdi="$(lfdi_of "$DERMS_DIR/$BAT.crt")"
  pv_lfdi="$(lfdi_of "$DERMS_DIR/$PV.crt")"
  fetch_ca
  write_config "$bat_lfdi" "$pv_lfdi"
  pair_device "$agg_lfdi" "$bat_lfdi"
  pair_device "$agg_lfdi" "$pv_lfdi"
  build_client
  echo "to request a flow reservation from another terminal:"
  echo "  $0 reserve [--energy-wh N] [--power-w N] [--start-in-s N] [--duration-s N]"
  echo "to withdraw it: $0 withdraw"
  # The pid file holds this shell's pid, which the exec below turns into the
  # client's pid. Ctrl-C reaches the client directly; a stale file after an
  # exit is detected by running_pid, not trusted.
  local pidtmp
  pidtmp="$(mktemp "$DERMS_DIR/.aggregator.pid.XXXXXX")" || die "cannot create a temporary pid file"
  echo $$ >"$pidtmp"
  mv -f "$pidtmp" "$DERMS_DIR/aggregator.pid"
  # exec skips the EXIT trap, so drop the temporary file first.
  rm -f "$ADMIN_BODY"
  trap - EXIT
  # The generated config must be the whole configuration: env -u keeps an
  # exported SEP2_SERVER, SEP2_CERT, SEP2_KEY or SEP2_CA from overriding it.
  exec env -u SEP2_SERVER -u SEP2_CERT -u SEP2_KEY -u SEP2_CA \
    "$DERMS_DIR/inverterclient" --sim-config "$DERMS_DIR/$AGG.json"
}

# running_pid prints the pid of the running aggregator, or fails. A pid file
# naming some other program is refused, because SIGUSR1 would terminate it:
# the process must be an inverterclient whose command line carries this
# aggregator's generated config, since a standalone device is an
# inverterclient too and registers no SIGUSR1 handler.
running_pid() {
  local pidfile="$DERMS_DIR/aggregator.pid" pid comm arg found=0 cmdline=()
  [[ -r "$pidfile" ]] || die "no pid file $pidfile: is the aggregator started (start-aggregator.sh start)?"
  pid="$(<"$pidfile")"
  [[ "$pid" =~ ^[0-9]+$ ]] || die "pid file $pidfile does not hold a pid"
  comm="$(cat "/proc/$pid/comm" 2>/dev/null)" || die "process $pid is not running (stale pid file $pidfile)"
  [[ "$comm" == inverterclient ]] || die "process $pid is '$comm', not inverterclient (stale pid file $pidfile)"
  mapfile -d '' -t cmdline <"/proc/$pid/cmdline" 2>/dev/null || die "cannot read the command line of process $pid (stale pid file $pidfile)"
  for arg in "${cmdline[@]}"; do
    [[ "$arg" == "$DERMS_DIR/$AGG.json" ]] && found=1
  done
  ((found)) || die "process $pid is an inverterclient but not the aggregator started from $DERMS_DIR/$AGG.json (stale pid file $pidfile)"
  printf '%s\n' "$pid"
}

# write_request writes the overrides atomically: a temporary file in the
# same directory, then a rename, so the aggregator never reads half a file.
write_request() {
  local json="$1" tmp
  tmp="$(mktemp "$DERMS_DIR/.frq-request.XXXXXX")" || die "cannot create a temporary request file"
  printf '%s\n' "$json" >"$tmp"
  mv -f "$tmp" "$DERMS_DIR/frq-request.json" || {
    rm -f "$tmp"
    die "cannot place the request file"
  }
}

cmd_reserve() {
  local energy="" power="" start_in="" duration=""
  while (($# > 0)); do
    case "$1" in
      --energy-wh | --power-w | --start-in-s | --duration-s)
        (($# >= 2)) || die "$1 needs a value"
        case "$1" in
          --energy-wh) energy="$2" ;;
          --power-w) power="$2" ;;
          --start-in-s) start_in="$2" ;;
          --duration-s) duration="$2" ;;
        esac
        shift 2
        ;;
      *) die "unknown reserve option '$1'" ;;
    esac
  done
  local num='^[0-9]+([.][0-9]+)?$' snum='^-?[0-9]+([.][0-9]+)?$' zero='^-?0+([.]0+)?$' int='^[0-9]+$'
  # Positive energy charges the battery, negative discharges it (the client's
  # convention); only zero is refused.
  [[ -z "$energy" || ( "$energy" =~ $snum && ! "$energy" =~ $zero ) ]] ||
    die "--energy-wh must be a non-zero number (positive charges, negative discharges)"
  [[ -z "$power" || "$power" =~ $num ]] || die "--power-w must be a positive number"
  [[ -z "$start_in" || "$start_in" =~ $int ]] || die "--start-in-s must be a whole number of seconds"
  [[ -z "$duration" || "$duration" =~ $int ]] || die "--duration-s must be a whole number of seconds"
  local pid
  pid="$(running_pid)"
  # Only the given fields are written; jq checks each is a number.
  local json
  json="$(jq -nc \
    --arg e "$energy" --arg p "$power" --arg s "$start_in" --arg d "$duration" \
    '{energy_wh: $e, power_w: $p, start_in_s: $s, duration_s: $d}
     | with_entries(select(.value != "")) | map_values(tonumber)')" || die "cannot build the request"
  write_request "$json"
  kill -USR1 "$pid" || die "cannot signal process $pid"
  local waited=0 limit="${PICKUP_WAIT_S:-10}"
  while [[ -e "$DERMS_DIR/frq-request.json" ]]; do
    if ((waited >= limit)); then
      rm -f "$DERMS_DIR/frq-request.json"
      die "request not picked up within ${limit} s: is the aggregator an up-to-date build? (restart it with start-aggregator.sh start)"
    fi
    sleep 1
    waited=$((waited + 1))
  done
  echo "request file read by process $pid: $json (check the aggregator log for whether it accepted the request)"
}

cmd_withdraw() {
  local pid
  pid="$(running_pid)"
  kill -USR2 "$pid" || die "cannot signal process $pid"
  echo "withdraw signal sent to process $pid; the aggregator logs the outcome"
}

cmd_unpair() {
  require_admin_key
  require_safe_admin_url
  need_tool openssl
  need_tool sha256sum
  need_tool curl
  require_files "$DERMS_DIR/$BAT.crt" "$DERMS_DIR/$PV.crt"
  unpair_device "$(lfdi_of "$DERMS_DIR/$BAT.crt")"
  unpair_device "$(lfdi_of "$DERMS_DIR/$PV.crt")"
}

main() {
  local sub="${1:-start}"
  (($# == 0)) || shift
  derms_dir_init
  case "$sub" in
    start | unpair | reserve | withdraw) ;;
    *) die "usage: $0 [start|reserve|withdraw|unpair]" ;;
  esac
  need_tool curl
  # The response body of each admin call lands here; removed on every exit.
  ADMIN_BODY="$(mktemp "$DERMS_DIR/.admin-body.XXXXXX")" || die "cannot create a temporary file"
  trap 'rm -f "$ADMIN_BODY"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "cmd_$sub" "$@"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
