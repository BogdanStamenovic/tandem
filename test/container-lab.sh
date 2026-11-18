#!/usr/bin/env bash
# A laptop stand-in for testing the client role against a real relay on
# this machine: a booted systemd-nspawn container whose root is tmpfs and
# whose /usr is this host's, overlaid so it is writable (setup installs into
# /usr/local). Nothing is written to the host's disk except an empty mount
# point under $LAB.
#
#   test/container-lab.sh up      boot it, NAT it out of $WAN, install ownbox in it
#   test/container-lab.sh shell   interactive root shell (GH_TOKEN exported)
#   test/container-lab.sh down    stop it and undo every host change
#
# The relay must allow 10.99.0.0/24 while the lab runs:
#   sudo tandem setup --non-interactive --allow 192.168.1.0/24,10.99.0.0/24 --enable yes
# and the allow list should be put back afterwards.
set -euo pipefail
LAB=${LAB:-/mnt/offload/tandem-ct}
WAN=${WAN:-$(ip -j route get 1.1.1.1 | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["dev"])')}
M=tandemct
ct() { sudo systemd-run -M $M --wait --pipe -q "$@"; }

case "${1:-}" in
up)
  sudo mkdir -p "$LAB/usr"
  cat /proc/sys/net/ipv4/ip_forward > /tmp/tandem-lab.ip_forward
  tmux new-session -d -s tandem-ct -x 200 -y 50 \
    "sudo systemd-nspawn -D $LAB --volatile=yes --overlay=/usr::/usr --bind-ro=/etc/ssl --bind-ro=/etc/ca-certificates -b -M $M --network-veth systemd.firstboot=off"
  for i in $(seq 60); do ct /usr/bin/true 2>/dev/null && break; sleep 1; done
  sudo ip addr add 10.99.0.1/24 dev ve-$M; sudo ip link set ve-$M up
  sudo sysctl -qw net.ipv4.ip_forward=1
  sudo nft -f - <<N
table ip tandem_lab {
  chain post { type nat hook postrouting priority srcnat; policy accept;
    ip saddr 10.99.0.0/24 oifname "$WAN" masquerade
  }
}
N
  ct /usr/bin/sh -c 'ip addr add 10.99.0.2/24 dev host0; ip link set host0 up; ip route add default via 10.99.0.1
    echo nameserver 1.1.1.1 > /etc/resolv.conf
    python3 -m venv /opt/ownbox && /opt/ownbox/bin/pip -q install git+https://github.com/BogdanStamenovic/ownbox.git
    mkdir -p /root/.cache/ownbox'
  sudo machinectl copy-to $M "$HOME/.cache/ownbox/catalog.json" /root/.cache/ownbox/catalog.json
  # ownbox clones with gh; the token stays inside this machine.
  (umask 077; gh auth token > /tmp/tandem-lab.tok; sudo machinectl copy-to $M /tmp/tandem-lab.tok /root/.ghtoken; rm -f /tmp/tandem-lab.tok)
  echo "lab up; run: $0 shell"
  ;;
shell)
  sudo machinectl shell $M /usr/bin/bash -c 'export HOME=/root GH_TOKEN=$(cat /root/.ghtoken); alias ownbox=/opt/ownbox/bin/ownbox; cd; exec bash -i'
  ;;
down)
  sudo machinectl poweroff $M 2>/dev/null || true
  for i in $(seq 30); do sudo machinectl status $M >/dev/null 2>&1 || break; sleep 1; done
  tmux kill-session -t tandem-ct 2>/dev/null || true
  sudo nft delete table ip tandem_lab 2>/dev/null || true
  [ -f /tmp/tandem-lab.ip_forward ] && sudo sysctl -qw net.ipv4.ip_forward="$(cat /tmp/tandem-lab.ip_forward)" && rm -f /tmp/tandem-lab.ip_forward
  mountpoint -q "$LAB/usr" && sudo umount "$LAB/usr" || true
  sudo rmdir "$LAB/usr" "$LAB" 2>/dev/null || true
  echo "lab down; ip_forward=$(cat /proc/sys/net/ipv4/ip_forward)"
  ;;
*) echo "usage: $0 up|shell|down" >&2; exit 2 ;;
esac
