#!/usr/bin/env bash
# Run the standalone simulated DER device for the manual DERMS test.
# Usage: start-device.sh
# Environment: DERMS_DIR (default ~/derms-certs), SEP2_URL
# (default https://127.0.0.1:8443), DEVICE (default solo-pv-1), PIN
# (default 111115, the registration PIN Recipe 1 sets).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$SCRIPT_DIR/lib.sh"

main() {
  need_tool openssl
  need_tool sha256sum
  derms_dir_init
  local device="${DEVICE:-solo-pv-1}"
  # The name becomes part of file paths, so only plain names pass.
  [[ "$device" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "DEVICE '$device' is not a plain name"
  local pin="${PIN:-111115}"
  [[ "$pin" =~ ^[0-9]+$ ]] || die "PIN must be a number"
  local cfg="$DERMS_REPO/sim/derms/$device.example.json"
  [[ -r "$cfg" ]] || die "no sim config $cfg for DEVICE '$device'"
  require_files "$DERMS_DIR/$device.crt" "$DERMS_DIR/$device.key" "$DERMS_DIR/ca.crt"
  build_client
  # Exec so Ctrl-C reaches the client directly and its own shutdown runs.
  exec "$DERMS_DIR/inverterclient" --sim-config "$cfg" \
    --server "${SEP2_URL:-https://127.0.0.1:8443}" \
    --cert "$DERMS_DIR/$device.crt" --key "$DERMS_DIR/$device.key" --ca "$DERMS_DIR/ca.crt" \
    --pin "$pin"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
