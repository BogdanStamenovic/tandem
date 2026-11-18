#!/usr/bin/env bash
# Upgrade-and-rollback test for the discovery migration, in the container
# lab (test/container-lab.sh up first). It does what ownbox does, from a git
# bundle of this repo, so nothing has to be pushed:
#   update   = check out the new revision, run its deploy/lifecycle.sh update
#   rollback = check out the old revision, run *its* deploy/lifecycle.sh update
#
#   test/migrate-ct.sh OLD_REV NEW_REV
set -uo pipefail
OLD=${1:?old revision}; NEW=${2:?new revision}
M=tandemct
here=$(cd "$(dirname "$0")/.." && pwd)
PASS=0; FAIL=0
ok()  { echo "PASS: $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL: $*"; FAIL=$((FAIL+1)); }
ct()  { sudo systemd-run -M $M --wait --pipe -q -E HOME=/root -E PATH=/usr/local/bin:/usr/bin /usr/bin/bash -c "$*"; }

B=$(mktemp -d)/tandem.bundle
git -C "$here" bundle create "$B" --all >/dev/null 2>&1
ct 'rm -f /root/tandem.bundle'   # copy-to will not overwrite
sudo machinectl copy-to $M "$B" /root/tandem.bundle || { echo "copy-to failed"; exit 1; }
ct 'rm -rf /root/tandem && git clone -q /root/tandem.bundle /root/tandem'
co()  { ct "cd /root/tandem && git -c advice.detachedHead=false checkout -q $1 && git log -1 --format=%h"; }
# The test restarts the unit more often than any real use; keep systemd's
# start limit out of the result.
upd() { ct "systemctl reset-failed 'tandem-*' 2>/dev/null; cd /root/tandem && ./deploy/lifecycle.sh update </dev/null 2>&1 | tail -6"; }
hash() { ct "sha256sum $1 | cut -c1-16"; }

for role in relay client; do
  echo "=== $role"
  ct 'cd /root/tandem; [ -x bin/tandem ] && TANDEM_PURGE=yes bin/tandem teardown --non-interactive >/dev/null 2>&1; rm -rf /etc/tandem /var/lib/tandem; true'
  echo "old: $(co $OLD)"
  if [ $role = relay ]; then
    ct 'cd /root/tandem && TANDEM_ROLE=relay TANDEM_LISTEN=10.99.0.2:7411 TANDEM_ALLOW=10.99.0.0/24 TANDEM_ENABLE=yes ./deploy/lifecycle.sh setup </dev/null 2>&1 | tail -3'
  else
    # A relay nobody answers at: the client is installed and enabled by force.
    ct "cd /root/tandem && TANDEM_ROLE=client TANDEM_RELAY=10.99.0.1:7411 TANDEM_PSK=$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n') TANDEM_ENABLE=yes ./deploy/lifecycle.sh setup </dev/null 2>&1 | tail -3"
    ct 'systemctl stop tandem-client; echo "{\"marker\":\"learned\"}" > /var/lib/tandem/stats.json.marker; cp /var/lib/tandem/stats.json /var/lib/tandem/stats.json.orig 2>/dev/null; systemctl start tandem-client'
  fi
  unit=tandem-$role
  ct "systemctl is-active $unit" | grep -qx active && ok "$role: old revision installed and running" || bad "$role: old install not running"
  k0=$(hash /etc/tandem/psk); c0=$(ct 'cat /etc/tandem/tandem.json')

  echo "update -> $(co $NEW)"; upd
  ct "systemctl is-active $unit" | grep -qx active && ok "$role: running after update" || { bad "$role: not running after update"; ct "journalctl -u $unit -n 10 --no-pager"; }
  [ "$(hash /etc/tandem/psk)" = "$k0" ] && ok "$role: key unchanged" || bad "$role: key changed"
  c1=$(ct 'cat /etc/tandem/tandem.json')
  if [ $role = relay ]; then
    echo "$c1" | grep -q '"listen": "0.0.0.0:7411"' && ok "relay: listen migrated to 0.0.0.0:7411" || bad "relay: listen is $(echo "$c1" | grep listen)"
    ct 'test -f /etc/tandem/tandem.json.pre-discovery && grep -q 10.99.0.2:7411 /etc/tandem/tandem.json.pre-discovery' && ok "relay: original config kept as .pre-discovery" || bad "relay: no .pre-discovery backup"
    sleep 6
    ct '/usr/local/lib/tandem/tandem status' | grep -q 'beacons from 10.99.0.2/24' && ok "relay: beaconing from 10.99.0.2" || { bad "relay: not beaconing"; ct '/usr/local/lib/tandem/tandem status'; }
  else
    [ "$c1" = "$c0" ] && ok "client: config untouched (relay stays as the static fallback)" || bad "client: config changed"
    ct 'test -f /var/lib/tandem/stats.json.marker' && ok "client: learned data directory kept" || bad "client: state lost"
    ct 'test ! -f /var/lib/tandem/discovery.json' && ok "client: relay did not answer, so no home recorded" || bad "client: home recorded without a relay"
    ct '/usr/local/lib/tandem/tandem status' | grep -q 'no relay on this network' && ok "client: status says no relay on this network" || { bad "client status"; ct '/usr/local/lib/tandem/tandem status'; }
  fi
  echo "update again (idempotent)"; upd >/dev/null
  [ "$(ct 'cat /etc/tandem/tandem.json')" = "$c1" ] && ok "$role: second update changes nothing" || bad "$role: second update changed config"

  echo "rollback -> $(co $OLD)"; upd
  ct "systemctl is-active $unit" | grep -qx active && ok "$role: old revision runs on the migrated config" || { bad "$role: rollback broke it"; ct "journalctl -u $unit -n 10 --no-pager"; }
  [ "$(hash /etc/tandem/psk)" = "$k0" ] && ok "$role: key unchanged after rollback" || bad "$role: key changed on rollback"
  if [ $role = relay ]; then
    ct 'ss -ltn' | grep -q '0.0.0.0:7411' && ok "relay: old binary listens on 0.0.0.0:7411" || bad "relay: old binary not listening"
  fi
  echo "forward again -> $(co $NEW)"; upd >/dev/null
  ct "systemctl is-active $unit" | grep -qx active && ok "$role: forward again after rollback" || bad "$role: forward after rollback"
done
echo "PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
