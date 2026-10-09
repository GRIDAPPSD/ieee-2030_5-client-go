#!/usr/bin/env bats
bats_require_minimum_version 1.5.0

# Tests for scripts/derms. No live server and no real aggregator: curl, go and
# inverterclient are stubs on PATH. Run with TMPDIR pointing at a scratch
# directory, because bats roots its temp directory there.

SCRIPTS="$(cd "$BATS_TEST_DIRNAME/.." && pwd -P)"
AGG="$SCRIPTS/start-aggregator.sh"
DEV="$SCRIPTS/start-device.sh"

make_cert() { # name
  openssl req -x509 -newkey rsa:2048 -nodes -keyout "$DERMS_DIR/$1.key" \
    -out "$DERMS_DIR/$1.crt" -days 2 -subj "/CN=$1" 2>/dev/null
}

# expected_lfdi derives the LFDI by a different route from the script: the
# openssl SHA-256 fingerprint, colons stripped.
expected_lfdi() {
  local fp
  fp="$(openssl x509 -in "$1" -noout -fingerprint -sha256)"
  fp="${fp#*=}"
  fp="${fp//:/}"
  printf '%s\n' "${fp:0:40}"
}

setup() {
  export DERMS_DIR="$BATS_TEST_TMPDIR/derms"
  mkdir -p "$DERMS_DIR" "$BATS_TEST_TMPDIR/bin"
  chmod 700 "$DERMS_DIR"
  export FAKE_LOG="$BATS_TEST_TMPDIR/curl.log"
  export FAKE_ROUTES="$BATS_TEST_TMPDIR/routes"
  : >"$FAKE_LOG"
  : >"$FAKE_ROUTES"
  export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
  export GO="$BATS_TEST_TMPDIR/bin/fakego"
  export FAKE_CLIENT_LOG="$BATS_TEST_TMPDIR/client.log"
  export PICKUP_WAIT_S=2
  unset SEP2_ADMIN_UI_KEY SEP2_SERVER SEP2_CERT SEP2_KEY SEP2_CA
  export SEP2_ADMIN_KEY=admin-test-key
  cat >"$BATS_TEST_TMPDIR/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Fake curl: logs argv and stdin config, answers from $FAKE_ROUTES lines of
# "METHOD PATH CODE BODY".
out="" method="GET" cfg=0 args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    -o) out="${args[i + 1]}" ;;
    -X) method="${args[i + 1]}" ;;
    -K) cfg=1 ;;
  esac
done
url="${args[${#args[@]} - 1]}"
path="/${url#*://*/}"
echo "$method $path ARGV: $*" >>"$FAKE_LOG"
if ((cfg)); then cat >>"$FAKE_LOG.stdin"; fi
while read -r m p code body; do
  if [[ "$m" == "$method" && "$p" == "$path" ]]; then
    if [[ "$body" == @* ]]; then cp "${body#@}" "$out"; else printf '%s' "$body" >"$out"; fi
    printf '%s' "$code"
    exit 0
  fi
done <"$FAKE_ROUTES"
printf 'no route %s %s\n' "$method" "$path" >>"$FAKE_LOG"
exit 7
STUB
  cat >"$BATS_TEST_TMPDIR/bin/fakego" <<'STUB'
#!/usr/bin/env bash
# Fake go: "go -C DIR build -o OUT PKG" writes a stub client at OUT.
echo "go $*" >>"$FAKE_CLIENT_LOG"
out=""
for ((i = 1; i <= $#; i++)); do
  if [[ "${!i}" == -o ]]; then j=$((i + 1)); out="${!j}"; fi
done
cat >"$out" <<'CLIENT'
#!/usr/bin/env bash
echo "client pid=$$ args: $*" >>"$FAKE_CLIENT_LOG"
echo "client env: SEP2_SERVER=${SEP2_SERVER-unset} SEP2_CERT=${SEP2_CERT-unset} SEP2_KEY=${SEP2_KEY-unset} SEP2_CA=${SEP2_CA-unset}" >>"$FAKE_CLIENT_LOG"
CLIENT
chmod +x "$out"
STUB
  chmod +x "$BATS_TEST_TMPDIR/bin/curl" "$BATS_TEST_TMPDIR/bin/fakego"
  make_cert agg-1
  make_cert agg-bat-1
  make_cert agg-pv-1
}

teardown() {
  if [[ -n "${FAKE_PID:-}" ]]; then kill "$FAKE_PID" 2>/dev/null || true; wait "$FAKE_PID" 2>/dev/null || true; fi
}

# start_fake_client launches a stub named inverterclient that handles the two
# signals the way the real one is documented to: SIGUSR1 consumes the
# request file (when FAKE_PICKUP=1), SIGUSR2 logs.
start_fake_client() {
  local args=("$@")
  ((${#args[@]} > 0)) || args=(--sim-config "$DERMS_DIR/agg-1.json")
  mkdir -p "$BATS_TEST_TMPDIR/fake"
  # /bin/bash directly: through env the kernel's process name becomes bash.
  cat >"$BATS_TEST_TMPDIR/fake/inverterclient" <<'STUB'
#!/bin/bash
trap 'cp "$DERMS_DIR/frq-request.json" "$FAKE_CLIENT_LOG.req"; [[ "${FAKE_PICKUP:-1}" == 1 ]] && rm -f "$DERMS_DIR/frq-request.json"; echo USR1 >>"$FAKE_CLIENT_LOG"' USR1
trap 'echo USR2 >>"$FAKE_CLIENT_LOG"' USR2
while :; do sleep 0.05; done
STUB
  chmod +x "$BATS_TEST_TMPDIR/fake/inverterclient"
  # fd 3 closed so bats does not wait on the background stub.
  "$BATS_TEST_TMPDIR/fake/inverterclient" "${args[@]}" 3>&- &
  FAKE_PID=$!
  echo "$FAKE_PID" >"$DERMS_DIR/aggregator.pid"
  sleep 0.3
}

routes_ok() {
  local bat pv
  bat="$(expected_lfdi "$DERMS_DIR/agg-bat-1.crt")"
  pv="$(expected_lfdi "$DERMS_DIR/agg-pv-1.crt")"
  {
    jq -nc --rawfile c "$DERMS_DIR/agg-1.crt" '{certPEM: $c}' >"$BATS_TEST_TMPDIR/ca.json"
    echo "GET /api/certs/ca 200 @$BATS_TEST_TMPDIR/ca.json"
    echo "POST /api/management-pairs 201 {}"
    echo "DELETE /api/management-pairs?managed=${bat^^} 204 -"
    echo "DELETE /api/management-pairs?managed=${pv^^} 204 -"
  } >"$FAKE_ROUTES"
}

# --- argument dispatch ---

@test "unknown subcommand fails with usage and touches nothing" {
  run "$AGG" frobnicate
  [ "$status" -ne 0 ]
  [[ "$output" == *"usage:"* ]]
  [ ! -e "$DERMS_DIR/agg-1.json" ]
}

@test "reserve rejects an unknown option" {
  start_fake_client
  run "$AGG" reserve --bogus 1
  [ "$status" -ne 0 ]
  [[ "$output" == *"unknown reserve option '--bogus'"* ]]
}

@test "reserve rejects a non-numeric value and a missing value" {
  start_fake_client
  run "$AGG" reserve --energy-wh abc
  [ "$status" -ne 0 ]
  [[ "$output" == *"--energy-wh must be a non-zero number"* ]]
  run "$AGG" reserve --start-in-s 1.5
  [ "$status" -ne 0 ]
  [[ "$output" == *"--start-in-s must be a whole number"* ]]
  run "$AGG" reserve --power-w
  [ "$status" -ne 0 ]
  [[ "$output" == *"--power-w needs a value"* ]]
  [ ! -e "$DERMS_DIR/frq-request.json" ]
}

# --- DERMS_DIR boundary ---

@test "DERMS_DIR inside the checkout is refused" {
  export DERMS_DIR="$SCRIPTS/../../sim/derms/should-not-exist"
  run "$AGG" unpair
  [ "$status" -ne 0 ]
  [[ "$output" == *"is inside the checkout"* ]]
  [ ! -e "$SCRIPTS/../../sim/derms/should-not-exist" ] || {
    rmdir "$SCRIPTS/../../sim/derms/should-not-exist"
    false
  }
}

@test "DERMS_DIR reaching the checkout through a symlink is refused" {
  ln -s "$SCRIPTS/.." "$BATS_TEST_TMPDIR/link"
  export DERMS_DIR="$BATS_TEST_TMPDIR/link"
  run "$AGG" unpair
  [ "$status" -ne 0 ]
  [[ "$output" == *"is inside the checkout"* ]]
}

@test "a sibling sharing the checkout's name prefix is allowed, the checkout itself is not" {
  source "$SCRIPTS/lib.sh"
  mkdir -p "$BATS_TEST_TMPDIR/repo"
  DERMS_REPO="$BATS_TEST_TMPDIR/repo"
  DERMS_DIR="$BATS_TEST_TMPDIR/repo-x/d" run derms_dir_init
  [ "$status" -eq 0 ]
  DERMS_DIR="$BATS_TEST_TMPDIR/repo/d" run derms_dir_init
  [ "$status" -ne 0 ]
  [[ "$output" == *"is inside the checkout"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/repo/d" ]
}

@test "a new DERMS_DIR is created private" {
  source "$SCRIPTS/lib.sh"
  DERMS_DIR="$BATS_TEST_TMPDIR/new/deep"
  derms_dir_init
  [ "$(stat -c %a "$DERMS_DIR")" = 700 ]
}

# --- LFDI derivation ---

@test "lfdi_of equals the SHA-256 fingerprint prefix, upper case, 40 hex" {
  source "$SCRIPTS/lib.sh"
  run lfdi_of "$DERMS_DIR/agg-bat-1.crt"
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9A-F]{40}$ ]]
  want="$(expected_lfdi "$DERMS_DIR/agg-bat-1.crt")"
  [ "$output" = "${want^^}" ]
}

@test "lfdi_of fails on a file that is not a certificate" {
  echo junk >"$DERMS_DIR/bad.crt"
  source "$SCRIPTS/lib.sh"
  run lfdi_of "$DERMS_DIR/bad.crt"
  [ "$status" -ne 0 ]
}

# --- start ---

@test "start with a missing certificate lists every missing file and starts nothing" {
  rm "$DERMS_DIR/agg-1.key" "$DERMS_DIR/agg-pv-1.crt"
  run "$AGG" start
  [ "$status" -ne 0 ]
  [[ "$output" == *"agg-1.key"* ]]
  [[ "$output" == *"agg-pv-1.crt"* ]]
  [ ! -s "$FAKE_LOG" ]
  [ ! -e "$DERMS_DIR/agg-1.json" ]
}

@test "start writes the config with the managed LFDIs, pairs both, and runs the client" {
  routes_ok
  run "$AGG" start
  [ "$status" -eq 0 ]
  bat="$(expected_lfdi "$DERMS_DIR/agg-bat-1.crt")"
  pv="$(expected_lfdi "$DERMS_DIR/agg-pv-1.crt")"
  agg="$(expected_lfdi "$DERMS_DIR/agg-1.crt")"
  cfg="$DERMS_DIR/agg-1.json"
  [ "$(jq -r .role "$cfg")" = aggregator ]
  [ "$(jq -r '.managed[0].name' "$cfg")" = agg-bat-1 ]
  [ "$(jq -r '.managed[0].lfdi' "$cfg")" = "${bat^^}" ]
  [ "$(jq -r '.managed[1].lfdi' "$cfg")" = "${pv^^}" ]
  [ "$(jq -r '.managed[0].device.type' "$cfg")" = battery ]
  [ "$(jq -r '.managed[1].device.type' "$cfg")" = pv ]
  [ "$(jq -r '.frq.request_file' "$cfg")" = "$DERMS_DIR/frq-request.json" ]
  [ "$(jq -r '.managed[0].replay.file' "$cfg")" = "$(cd "$SCRIPTS/../.." && pwd -P)/sim/derms/recordings/agg-bat-1.csv" ]
  # two pair POSTs, each naming the aggregator as manager
  [ "$(/usr/bin/grep -c '^POST /api/management-pairs' "$FAKE_LOG")" -eq 2 ]
  /usr/bin/grep -q "\"managerLFDI\":\"${agg^^}\"" "$FAKE_LOG"
  /usr/bin/grep -q "\"managedLFDI\":\"${bat^^}\"" "$FAKE_LOG"
  /usr/bin/grep -q "\"managedLFDI\":\"${pv^^}\"" "$FAKE_LOG"
  # the CA came from the API
  /usr/bin/grep -q '^GET /api/certs/ca' "$FAKE_LOG"
  [[ "$(<"$DERMS_DIR/ca.crt")" == *"BEGIN CERTIFICATE"* ]]
  # the client ran with the config, in the foreground (exec), and the pid file names it
  /usr/bin/grep -q "client pid=" "$FAKE_CLIENT_LOG"
  /usr/bin/grep -q -- "--sim-config $DERMS_DIR/agg-1.json" "$FAKE_CLIENT_LOG"
  cpid="$(sed -n 's/^client pid=\([0-9]*\) .*/\1/p' "$FAKE_CLIENT_LOG")"
  [ "$(<"$DERMS_DIR/aggregator.pid")" = "$cpid" ]
  # the build went to DERMS_DIR
  /usr/bin/grep -q -- "-o $DERMS_DIR/inverterclient" "$FAKE_CLIENT_LOG"
}

@test "start sends the admin key on stdin only and never prints it" {
  routes_ok
  export SEP2_ADMIN_KEY="s3cret-key-value"
  run "$AGG" start
  [ "$status" -eq 0 ]
  [[ "$output" != *"s3cret-key-value"* ]]
  run ! /usr/bin/grep -q "s3cret-key-value" "$FAKE_LOG"
  /usr/bin/grep -q "Authorization: Bearer s3cret-key-value" "$FAKE_LOG.stdin"
  run ! /usr/bin/grep -q "s3cret-key-value" "$DERMS_DIR/agg-1.json"
}

@test "start without an admin key refuses before any request, naming SEP2_ADMIN_KEY and make run" {
  routes_ok
  unset SEP2_ADMIN_KEY
  run "$AGG" start
  [ "$status" -ne 0 ]
  [[ "$output" == *"SEP2_ADMIN_KEY"* ]]
  [[ "$output" == *"make run"* ]]
  [ ! -s "$FAKE_LOG" ]
  [ ! -e "$FAKE_LOG.stdin" ]
  [ ! -e "$DERMS_DIR/agg-1.json" ]
}

@test "an empty SEP2_ADMIN_KEY is refused like an unset one" {
  routes_ok
  export SEP2_ADMIN_KEY=""
  run "$AGG" start
  [ "$status" -ne 0 ]
  [[ "$output" == *"SEP2_ADMIN_KEY"* ]]
  [ ! -s "$FAKE_LOG" ]
}

@test "unpair without an admin key refuses before any request" {
  routes_ok
  unset SEP2_ADMIN_KEY
  run "$AGG" unpair
  [ "$status" -ne 0 ]
  [[ "$output" == *"SEP2_ADMIN_KEY"* ]]
  [ ! -s "$FAKE_LOG" ]
}

@test "the retired SEP2_ADMIN_UI_KEY name is not accepted" {
  routes_ok
  unset SEP2_ADMIN_KEY
  export SEP2_ADMIN_UI_KEY="old-name-key"
  run "$AGG" start
  [ "$status" -ne 0 ]
  [[ "$output" == *"SEP2_ADMIN_KEY"* ]]
  [ ! -s "$FAKE_LOG" ]
}

@test "an admin key with a double quote, backslash or newline is refused before any request" {
  routes_ok
  local bad
  for bad in 'ab"cd' 'ab\cd' $'ab\ncd'; do
    SEP2_ADMIN_KEY="$bad" run "$AGG" start
    [ "$status" -ne 0 ]
    [[ "$output" == *"SEP2_ADMIN_KEY"* ]]
    [[ "$output" == *"double quote"* ]]
    [ ! -s "$FAKE_LOG" ]
  done
}

@test "curl gets a connect timeout and a total timeout" {
  routes_ok
  run "$AGG" start
  [ "$status" -eq 0 ]
  /usr/bin/grep -q -- '--connect-timeout [0-9]' "$FAKE_LOG"
  /usr/bin/grep -q -- '--max-time [0-9]' "$FAKE_LOG"
}

@test "a plain-http ADMIN_URL off loopback is refused before any request" {
  routes_ok
  local u
  for u in http://admin.example.com:8444 http://10.0.0.5:8444 http://127.0.0.1.evil.example:8444 http://127.0.0.1@evil.example:8444; do
    ADMIN_URL="$u" run "$AGG" start
    [ "$status" -ne 0 ]
    [[ "$output" == *"ADMIN_URL"* ]]
    [ ! -s "$FAKE_LOG" ]
  done
}

@test "loopback http and any https ADMIN_URL are accepted" {
  routes_ok
  local u
  for u in http://127.0.0.1:8444 http://localhost:8444 "http://[::1]:8444" https://admin.example.com:8444; do
    : >"$FAKE_LOG"
    ADMIN_URL="$u" run "$AGG" start
    [ "$status" -eq 0 ]
    /usr/bin/grep -q '^GET /api/certs/ca' "$FAKE_LOG"
  done
}

@test "an existing DERMS_DIR writable by group or others is refused" {
  local m
  for m in 770 707 775 777 720; do
    chmod "$m" "$DERMS_DIR"
    run "$AGG" unpair
    [ "$status" -ne 0 ]
    [[ "$output" == *"writable by group or others"* ]]
  done
  chmod 700 "$DERMS_DIR"
}

@test "an existing DERMS_DIR with mode 750 or 755 is accepted" {
  routes_ok
  chmod 750 "$DERMS_DIR"
  run "$AGG" unpair
  [ "$status" -eq 0 ]
}

@test "an existing DERMS_DIR owned by someone else is refused" {
  mkdir -p "$BATS_TEST_TMPDIR/statbin"
  printf '#!/usr/bin/env bash\necho "0 700"\n' >"$BATS_TEST_TMPDIR/statbin/stat"
  chmod +x "$BATS_TEST_TMPDIR/statbin/stat"
  PATH="$BATS_TEST_TMPDIR/statbin:$PATH" run "$AGG" unpair
  [ "$status" -ne 0 ]
  [[ "$output" == *"not owned by you"* ]]
}

@test "start does not write through symlinks planted at the old fixed temp names" {
  routes_ok
  : >"$BATS_TEST_TMPDIR/victim"
  ln -s "$BATS_TEST_TMPDIR/victim" "$DERMS_DIR/ca.crt.tmp"
  ln -s "$BATS_TEST_TMPDIR/victim" "$DERMS_DIR/agg-1.json.tmp"
  ln -s "$BATS_TEST_TMPDIR/victim" "$DERMS_DIR/aggregator.pid"
  run "$AGG" start
  [ "$status" -eq 0 ]
  [ ! -s "$BATS_TEST_TMPDIR/victim" ]
  [ ! -L "$DERMS_DIR/aggregator.pid" ]
  cpid="$(sed -n 's/^client pid=\([0-9]*\) .*/\1/p' "$FAKE_CLIENT_LOG")"
  [ "$(<"$DERMS_DIR/aggregator.pid")" = "$cpid" ]
}

@test "exported SEP2_SERVER, SEP2_CERT, SEP2_KEY and SEP2_CA do not reach the aggregator" {
  routes_ok
  export SEP2_SERVER=https://elsewhere.example SEP2_CERT=/nope.crt SEP2_KEY=/nope.key SEP2_CA=/nope-ca.crt
  run "$AGG" start
  [ "$status" -eq 0 ]
  /usr/bin/grep -q -- '^client env: SEP2_SERVER=unset SEP2_CERT=unset SEP2_KEY=unset SEP2_CA=unset$' "$FAKE_CLIENT_LOG"
  /usr/bin/grep -q -- "--sim-config $DERMS_DIR/agg-1.json" "$FAKE_CLIENT_LOG"
}

@test "a 409 on pairing stops start before the client runs" {
  routes_ok
  sed -i 's|^POST /api/management-pairs 201|POST /api/management-pairs 409|' "$FAKE_ROUTES"
  run "$AGG" start
  [ "$status" -ne 0 ]
  [[ "$output" == *"already managed by another manager (409)"* ]]
  [ ! -e "$FAKE_CLIENT_LOG" ]
}

@test "an unexpected pairing status stops start naming the status" {
  routes_ok
  sed -i 's|^POST /api/management-pairs 201|POST /api/management-pairs 401|' "$FAKE_ROUTES"
  run "$AGG" start
  [ "$status" -ne 0 ]
  [[ "$output" == *"answered 401"* ]]
}

@test "a CA answer without certPEM stops start" {
  routes_ok
  sed -i 's|^GET /api/certs/ca 200 .*|GET /api/certs/ca 200 {}|' "$FAKE_ROUTES"
  run "$AGG" start
  [ "$status" -ne 0 ]
  [[ "$output" == *"no certPEM"* ]]
  [ ! -e "$DERMS_DIR/agg-1.json" ]
}

@test "start leaves no temporary files behind" {
  routes_ok
  run "$AGG" start
  [ "$status" -eq 0 ]
  [ -z "$(ls -A "$DERMS_DIR" | /usr/bin/grep -E '\.tmp$|^\.admin-body|^\.frq-request')" ]
}

# --- unpair ---

@test "unpair deletes both pairs and accepts 404 for a pair already gone" {
  routes_ok
  pv="$(expected_lfdi "$DERMS_DIR/agg-pv-1.crt")"
  sed -i "s|^DELETE /api/management-pairs?managed=${pv^^} 204|DELETE /api/management-pairs?managed=${pv^^} 404|" "$FAKE_ROUTES"
  run "$AGG" unpair
  [ "$status" -eq 0 ]
  [[ "$output" == *"unpaired"* ]]
  [[ "$output" == *"already gone"* ]]
  [ "$(/usr/bin/grep -c '^DELETE ' "$FAKE_LOG")" -eq 2 ]
}

@test "unpair fails on a 500" {
  routes_ok
  bat="$(expected_lfdi "$DERMS_DIR/agg-bat-1.crt")"
  sed -i "s|managed=${bat^^} 204|managed=${bat^^} 500|" "$FAKE_ROUTES"
  run "$AGG" unpair
  [ "$status" -ne 0 ]
  [[ "$output" == *"answered 500"* ]]
}

# --- reserve ---

@test "reserve writes only the given fields, as numbers, and signals the client" {
  start_fake_client
  run "$AGG" reserve --energy-wh 4000 --power-w 2000
  [ "$status" -eq 0 ]
  [[ "$output" == *"request file read by process"* ]]
  [ "$(jq -c . "$FAKE_CLIENT_LOG.req")" = '{"energy_wh":4000,"power_w":2000}' ]
  /usr/bin/grep -q USR1 "$FAKE_CLIENT_LOG"
  [ ! -e "$DERMS_DIR/frq-request.json" ]
}

@test "reserve accepts a negative energy (discharge) and writes it as a negative number" {
  start_fake_client
  run "$AGG" reserve --energy-wh -4000
  [ "$status" -eq 0 ]
  [ "$(jq -c . "$FAKE_CLIENT_LOG.req")" = '{"energy_wh":-4000}' ]
  run "$AGG" reserve --energy-wh -250.5
  [ "$status" -eq 0 ]
  [ "$(jq -c . "$FAKE_CLIENT_LOG.req")" = '{"energy_wh":-250.5}' ]
}

@test "reserve refuses zero energy in every spelling, and writes no request" {
  start_fake_client
  local z
  for z in 0 0.0 -0 -0.00 000; do
    run "$AGG" reserve --energy-wh "$z"
    [ "$status" -ne 0 ]
    [[ "$output" == *"--energy-wh must be a non-zero number"* ]]
    [ ! -e "$DERMS_DIR/frq-request.json" ]
  done
  run "$AGG" reserve --energy-wh -
  [ "$status" -ne 0 ]
  run "$AGG" reserve --energy-wh --5
  [ "$status" -ne 0 ]
}

@test "reserve still refuses a negative power" {
  start_fake_client
  run "$AGG" reserve --power-w -100
  [ "$status" -ne 0 ]
  [[ "$output" == *"--power-w must be a positive number"* ]]
}

@test "reserve with all four overrides writes all four keys the client reads" {
  start_fake_client
  run "$AGG" reserve --energy-wh 4000.5 --power-w 2000 --start-in-s 2400 --duration-s 3600
  [ "$status" -eq 0 ]
  [ "$(jq -S -c . "$FAKE_CLIENT_LOG.req")" = '{"duration_s":3600,"energy_wh":4000.5,"power_w":2000,"start_in_s":2400}' ]
}

@test "reserve with no overrides writes an empty object" {
  start_fake_client
  run "$AGG" reserve
  [ "$status" -eq 0 ]
  [ "$(jq -c . "$FAKE_CLIENT_LOG.req")" = '{}' ]
}

@test "the request file is written by rename from a temp file in the same directory" {
  start_fake_client
  # Wrap mv: log source, destination and whether the source already held
  # complete JSON at the moment of the rename.
  cat >"$BATS_TEST_TMPDIR/bin/mv" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do last="$a"; done
if [[ "$last" == */frq-request.json ]]; then
  src="${*: -2:1}"
  if jq -e . "$src" >/dev/null 2>&1; then ok=complete; else ok=partial; fi
  echo "mv src=$src dest=$last content=$ok" >>"$FAKE_CLIENT_LOG.mv"
fi
exec /usr/bin/mv "$@"
STUB
  chmod +x "$BATS_TEST_TMPDIR/bin/mv"
  run "$AGG" reserve --power-w 1500
  [ "$status" -eq 0 ]
  [ "$(wc -l <"$FAKE_CLIENT_LOG.mv")" -eq 1 ]
  /usr/bin/grep -q "^mv src=$DERMS_DIR/.frq-request\\.[A-Za-z0-9]* dest=$DERMS_DIR/frq-request.json content=complete$" "$FAKE_CLIENT_LOG.mv"
}

@test "reserve reports a request not picked up and removes the stale file" {
  start_fake_client
  export FAKE_PICKUP=0
  kill "$FAKE_PID"; wait "$FAKE_PID" 2>/dev/null || true
  FAKE_PID=
  # restart the stub with pickup off (the env is read by the stub's trap)
  start_fake_client
  run "$AGG" reserve --power-w 1000
  [ "$status" -ne 0 ]
  [[ "$output" == *"not picked up within 2 s"* ]]
  [ ! -e "$DERMS_DIR/frq-request.json" ]
}

@test "reserve with no pid file fails and writes no request" {
  run "$AGG" reserve --power-w 1000
  [ "$status" -ne 0 ]
  [[ "$output" == *"no pid file"* ]]
  [ ! -e "$DERMS_DIR/frq-request.json" ]
}

@test "reserve refuses a pid that is not an inverterclient and does not signal it" {
  sleep 30 3>&- &
  FAKE_PID=$!
  echo "$FAKE_PID" >"$DERMS_DIR/aggregator.pid"
  run "$AGG" reserve --power-w 1000
  [ "$status" -ne 0 ]
  [[ "$output" == *"not inverterclient"* ]]
  kill -0 "$FAKE_PID"
  [ ! -e "$DERMS_DIR/frq-request.json" ]
}

@test "reserve refuses an inverterclient that is not this aggregator and does not signal it" {
  start_fake_client --sim-config "$BATS_TEST_TMPDIR/some-other-device.json"
  run "$AGG" reserve --power-w 1000
  [ "$status" -ne 0 ]
  [[ "$output" == *"not the aggregator started from $DERMS_DIR/agg-1.json"* ]]
  sleep 0.3
  kill -0 "$FAKE_PID"
  run ! /usr/bin/grep -q USR1 "$FAKE_CLIENT_LOG"
  [ ! -e "$DERMS_DIR/frq-request.json" ]
  run "$AGG" withdraw
  [ "$status" -ne 0 ]
  sleep 0.3
  run ! /usr/bin/grep -q USR2 "$FAKE_CLIENT_LOG"
}

@test "reserve with a stale pid file fails naming it" {
  echo 999999 >"$DERMS_DIR/aggregator.pid"
  run "$AGG" reserve
  [ "$status" -ne 0 ]
  [[ "$output" == *"stale pid file"* ]]
}

# --- withdraw ---

@test "withdraw sends SIGUSR2 and not SIGUSR1" {
  start_fake_client
  run "$AGG" withdraw
  [ "$status" -eq 0 ]
  sleep 0.3
  /usr/bin/grep -q USR2 "$FAKE_CLIENT_LOG"
  run ! /usr/bin/grep -q USR1 "$FAKE_CLIENT_LOG"
}

@test "withdraw with no running aggregator fails" {
  run "$AGG" withdraw
  [ "$status" -ne 0 ]
  [[ "$output" == *"no pid file"* ]]
}

# --- start-device ---

@test "start-device with missing files lists them and runs nothing" {
  run "$DEV"
  [ "$status" -ne 0 ]
  [[ "$output" == *"solo-pv-1.crt"* ]]
  [[ "$output" == *"ca.crt"* ]]
  [ ! -e "$FAKE_CLIENT_LOG" ]
}

@test "start-device runs the standalone client with the sim config and its own certificate" {
  make_cert solo-pv-1
  cp "$DERMS_DIR/agg-1.crt" "$DERMS_DIR/ca.crt"
  run "$DEV"
  [ "$status" -eq 0 ]
  /usr/bin/grep -q -- "--sim-config .*sim/derms/solo-pv-1.example.json" "$FAKE_CLIENT_LOG"
  /usr/bin/grep -q -- "--cert $DERMS_DIR/solo-pv-1.crt --key $DERMS_DIR/solo-pv-1.key --ca $DERMS_DIR/ca.crt" "$FAKE_CLIENT_LOG"
  /usr/bin/grep -q -- "--pin 111115" "$FAKE_CLIENT_LOG"
}

@test "start-device refuses a DEVICE that is not a plain name" {
  DEVICE='../etc/passwd' run "$DEV"
  [ "$status" -ne 0 ]
  [[ "$output" == *"not a plain name"* ]]
}

@test "start-device refuses a DEVICE with no sim config" {
  DEVICE=other run "$DEV"
  [ "$status" -ne 0 ]
  [[ "$output" == *"no sim config"* ]]
}

@test "the scripts leave the checkout unchanged" {
  routes_ok
  local before
  before="$(git -C "$SCRIPTS" status --porcelain -- .)"
  run "$AGG" start
  [ "$status" -eq 0 ]
  [ "$(git -C "$SCRIPTS" status --porcelain -- .)" = "$before" ]
}
