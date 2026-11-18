#!/usr/bin/env bash
# Before/after throughput on a client with tandem running.
#
#   test/bench.sh [ROUNDS]          (default 2)
#
# For every round it pins all new connections to "direct", then to "relay"
# (tandem force), and measures:
#   - single-stream downloads: Hetzner fsn1, OVH, Hetzner Ashburn (US, long
#     RTT), and extra.files.tar.gz from the first mirror in the mirrorlist;
#   - a real `pacman -Sw` of a few large packages into a throwaway cache dir:
#     the download half of `yay -Syu`, with pacman's parallel downloads.
# Modes alternate within a round so drift (time of day, Wi-Fi) hits both.
# It always returns tandem to `force auto` on exit. Needs sudo for pacman.
set -uo pipefail
ROUNDS=${1:-2}
TANDEM=${TANDEM:-tandem}
PKGS=${PKGS:-"linux go python"}
HETZNER=${HETZNER:-https://fsn1-speed.hetzner.com/100MB.bin}
OVH=${OVH:-https://proof.ovh.net/files/100Mb.dat}
FAR=${FAR:-https://ash-speed.hetzner.com/100MB.bin}  # Ashburn, US: long RTT
# Cloudflare's speed endpoint returns 403 to plain curl (anti-bot); it is not
# worked around.
mirror=$(sed -n 's/^Server *= *//p' /etc/pacman.d/mirrorlist | head -1 | sed 's#\$repo#core#; s#\$arch#x86_64#')
MIRROR_FILE=${MIRROR_FILE:-$(echo "$mirror" | sed 's#/core/#/extra/#')/extra.files.tar.gz}  # ~50 MB
trap '$TANDEM force auto >/dev/null 2>&1' EXIT

mbit() { awk '{printf "%.0f", $1*8/1e6}'; }
dl() { curl -s -o /dev/null --max-time 120 -w '%{speed_download}' "$1" | mbit; }
pac() {
  local dir t0 t1 bytes
  dir=$(sudo mktemp -d)
  # pacman downloads as its sandbox user (DownloadUser = alpm), which must
  # be able to write the cache dir.
  id alpm >/dev/null 2>&1 && sudo chown alpm:alpm "$dir"
  t0=$(date +%s.%N)
  sudo pacman -Sw --noconfirm --cachedir "$dir" $PKGS >/dev/null 2>&1
  t1=$(date +%s.%N)
  bytes=$(sudo du -sb "$dir" | cut -f1)
  sudo rm -rf "$dir"
  echo "$bytes $t0 $t1" | awk '{printf "%.0f", $1*8/1e6/($3-$2)}'
}

$TANDEM status | sed -n 1,3p
echo "mirror: $MIRROR_FILE; pacman packages: $PKGS"
echo
echo "| round | mode | Hetzner fsn1 | OVH | Hetzner Ashburn | mirror file | pacman -Sw |"
echo "|---|---|---|---|---|---|---|"
for r in $(seq "$ROUNDS"); do
  order="direct relay"; [ $((r % 2)) = 0 ] && order="relay direct"
  for m in $order; do
    $TANDEM force "$m" >/dev/null || { echo "tandem force failed (daemon running?)"; exit 1; }
    echo "| $r | $m | $(dl "$HETZNER") | $(dl "$OVH") | $(dl "$FAR") | $(dl "$MIRROR_FILE") | $(pac) |"
  done
done
echo
echo "Mbit/s. pacman -Sw is total bytes over wall time with pacman's parallel downloads."
