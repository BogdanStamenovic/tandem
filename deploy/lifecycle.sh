#!/usr/bin/env bash
# ownbox lifecycle entry: setup | update | remove. Runs from the checkout.
#
# Builds as the invoking user, then hands the privileged part to
# `tandem setup|upgrade|teardown` under sudo. On a terminal sudo may ask for a
# password and tandem asks its questions; without one (ownbox --yes) sudo
# must be passwordless and tandem takes TANDEM_* variables or safe defaults.
set -euo pipefail
action=${1:?usage: lifecycle.sh setup|update|remove [--dry-run]}
shift || true
here=$(cd "$(dirname "$0")/.." && pwd)

build() {
  command -v go >/dev/null || { echo "tandem: Go is required to build (pacman -S go)" >&2; exit 1; }
  # Lifecycle commands may run with a bare environment (no HOME, e.g. from a
  # systemd unit); Go then has nowhere to put its build cache.
  if [ -z "${GOCACHE:-}" ] && [ -z "${HOME:-}" ] && [ -z "${XDG_CACHE_HOME:-}" ]; then
    export GOCACHE="$here/.gocache"
  fi
  echo "building tandem ($(git -C "$here" rev-parse --short HEAD 2>/dev/null || echo unknown))"
  # Vendored dependencies: no network needed, and the patched nftables copy is used.
  (cd "$here" && CGO_ENABLED=0 go build -mod=vendor -trimpath \
     -ldflags "-s -w -X main.version=$(git -C "$here" describe --always --dirty 2>/dev/null || echo dev)" \
     -o bin/tandem ./cmd/tandem)
}

as_root() {
  if [ "$(id -u)" = 0 ]; then "$@"; return; fi
  if [ -t 0 ]; then sudo -E "$@"; return; fi
  if ! sudo -n true 2>/dev/null; then
    echo "tandem: no terminal and sudo needs a password; rerun interactively: ownbox $action tandem" >&2
    exit 1
  fi
  sudo -n -E "$@"
}

case "$action" in
  setup)  build; as_root "$here/bin/tandem" setup "$@" ;;
  update) build; as_root "$here/bin/tandem" upgrade "$@" ;;
  remove)
    # The checkout's binary may be gone if a build never happened; the
    # installed copy can tear itself down too.
    b="$here/bin/tandem"; [ -x "$b" ] || b=/usr/local/lib/tandem/tandem
    if [ ! -x "$b" ]; then echo "tandem: nothing installed"; exit 0; fi
    as_root "$b" teardown "$@" ;;
  *) echo "unknown action $action" >&2; exit 2 ;;
esac
