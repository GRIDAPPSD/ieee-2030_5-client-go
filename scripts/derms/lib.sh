#!/usr/bin/env bash
# Shared helpers for start-aggregator.sh and start-device.sh. Sourced, never
# run. Nothing here writes inside the checkout: generated state lives under
# DERMS_DIR.

DERMS_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"

die() {
  echo "error: $*" >&2
  exit 1
}

need_tool() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required and not on PATH"
}

# derms_dir_init resolves DERMS_DIR, creates it with mode 0700 when absent,
# and refuses a directory inside the checkout, which would put generated
# keys and configs under version control's nose.
derms_dir_init() {
  local dir="${DERMS_DIR:-$HOME/derms-certs}"
  [[ -n "$dir" ]] || die "DERMS_DIR is empty"
  [[ "$dir" = /* ]] || dir="$PWD/$dir"
  # Canonicalize the nearest existing ancestor first (symlinks and .. are
  # resolved there), keep the not-yet-existing tail, and run the checkout test
  # BEFORE creating anything, so a refused directory is never created.
  local existing="$dir" tail=""
  while [[ ! -d "$existing" ]]; do
    tail="/$(basename "$existing")$tail"
    existing="$(dirname "$existing")"
  done
  [[ "$tail" != */../* && "$tail" != */.. && "$tail" != */./* && "$tail" != */. ]] ||
    die "DERMS_DIR $dir has a . or .. segment below an existing directory"
  existing="$(cd "$existing" && pwd -P)" || die "cannot enter $existing"
  [[ "$existing" == / ]] && existing=""
  dir="$existing$tail"
  # A trailing separator on both sides keeps a sibling such as <repo>-x out.
  case "$dir/" in
    "$DERMS_REPO"/*) die "DERMS_DIR $dir is inside the checkout $DERMS_REPO; choose a directory outside it" ;;
  esac
  if [[ ! -d "$dir" ]]; then
    # umask 077 makes every directory it creates private, parents included.
    (umask 077 && mkdir -p "$dir") || die "cannot create DERMS_DIR $dir"
  fi
  # Generated keys and configs live here, so the directory must be private to
  # this user: someone else's directory or a group- or world-writable one
  # would let another account plant files the scripts then read or replace.
  local owner_mode owner mode
  owner_mode="$(stat -c '%u %a' "$dir")" || die "cannot stat DERMS_DIR $dir"
  owner="${owner_mode%% *}"
  mode="${owner_mode##* }"
  [[ "$owner" == "$(id -u)" ]] || die "DERMS_DIR $dir is not owned by you; choose a directory you own"
  ((8#$mode & 8#022)) &&
    die "DERMS_DIR $dir is writable by group or others (mode $mode); run chmod go-w on it or choose another"
  DERMS_DIR="$dir"
}

# require_files fails naming every missing or unreadable file, not just the
# first, so one run tells the operator the whole list to fetch.
require_files() {
  local missing=() f
  for f in "$@"; do
    [[ -r "$f" && -s "$f" ]] || missing+=("$f")
  done
  if ((${#missing[@]} > 0)); then
    echo "error: missing or empty certificate files (see scripts/derms/README.md, Recipe 1):" >&2
    printf '  %s\n' "${missing[@]}" >&2
    exit 1
  fi
}

# lfdi_of prints the LFDI of a PEM certificate: the first 40 hex digits of
# the SHA-256 of its DER form, upper case.
lfdi_of() {
  local pem="$1" digest
  # pipefail in a subshell: an unreadable certificate must not leave the
  # SHA-256 of empty input standing in for a real LFDI, whatever the caller's
  # shell options are.
  digest="$(set -o pipefail && openssl x509 -in "$pem" -outform DER | sha256sum)" ||
    die "cannot read certificate $pem"
  digest="${digest:0:40}"
  printf '%s\n' "${digest^^}"
}

# build_client compiles inverterclient into DERMS_DIR, so a build leaves
# nothing in the checkout.
build_client() {
  "${GO:-go}" -C "$DERMS_REPO" build -o "$DERMS_DIR/inverterclient" ./cmd/inverterclient ||
    die "go build of inverterclient failed"
}
