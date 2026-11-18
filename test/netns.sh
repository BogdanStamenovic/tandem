#!/usr/bin/env bash
# Integration test in network namespaces on one machine. Needs root.
#
#   t-client (laptop)  192.168.77.3 --[wifi: netem delay+loss]-- t-router
#   t-relay  (server)  192.168.78.2 --[clean LAN]--------------- t-router
#   t-inet   (internet) 203.0.113.10 --[WAN: netem delay]-------- t-router
#
# 203.0.113.0/24 is TEST-NET-3: not in tandem's private list, so it is
# treated as a public destination.
set -uo pipefail
BIN=${BIN:-$(dirname "$0")/../bin/tandem}
BIN=$(readlink -f "$BIN")
WIFI_DELAY=${WIFI_DELAY:-3ms}
WIFI_LOSS=${WIFI_LOSS:-1%}
WAN_DELAY=${WAN_DELAY:-20ms}
SIZE_MB=${SIZE_MB:-64}
T=$(mktemp -d /tmp/tandem-test.XXXX)
PASS=0; FAIL=0
ok()   { echo "PASS: $*"; PASS=$((PASS+1)); }
bad()  { echo "FAIL: $*"; FAIL=$((FAIL+1)); }
inc()  { ip netns exec t-client "$@"; }

cleanup() {
  for p in $(cat "$T"/*.pid 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  for n in t-client t-relay t-inet t-router; do ip netns del $n 2>/dev/null; done
  if [ -n "${KEEP:-}" ]; then echo "logs kept in $T"; else rm -rf "$T"; fi
}
trap cleanup EXIT

setup_net() {
  for n in t-client t-relay t-inet t-router; do ip netns add $n; ip -n $n link set lo up; done
  ip link add c0 netns t-client type veth peer name rc netns t-router
  ip link add s0 netns t-relay  type veth peer name rs netns t-router
  ip link add i0 netns t-inet   type veth peer name ri netns t-router
  ip -n t-client addr add 192.168.77.3/24 dev c0; ip -n t-router addr add 192.168.77.1/24 dev rc
  ip -n t-relay  addr add 192.168.78.2/24 dev s0; ip -n t-router addr add 192.168.78.1/24 dev rs
  ip -n t-inet   addr add 203.0.113.10/24 dev i0; ip -n t-router addr add 203.0.113.1/24 dev ri
  for l in "t-client c0" "t-relay s0" "t-inet i0" "t-router rc" "t-router rs" "t-router ri"; do set -- $l; ip -n $1 link set $2 up; done
  ip -n t-client route add default via 192.168.77.1
  ip -n t-relay  route add default via 192.168.78.1
  ip -n t-inet   route add default via 203.0.113.1
  ip netns exec t-router sysctl -qw net.ipv4.ip_forward=1
  # wifi leg: both directions
  tc -n t-client qdisc add dev c0 root netem delay $WIFI_DELAY loss $WIFI_LOSS
  tc -n t-router qdisc add dev rc root netem delay $WIFI_DELAY loss $WIFI_LOSS
  # internet leg: both directions
  tc -n t-inet   qdisc add dev i0 root netem delay $WAN_DELAY limit 10000
  tc -n t-router qdisc add dev ri root netem delay $WAN_DELAY limit 10000
}

start_inet() {
  mkdir -p "$T/www"
  head -c $((SIZE_MB<<20)) /dev/urandom > "$T/www/big.bin"
  ip netns exec t-inet python3 -m http.server 80 --bind 203.0.113.10 --directory "$T/www" >/dev/null 2>&1 &
  echo $! > "$T/http.pid"
  ip netns exec t-inet python3 -c '
import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(("203.0.113.10",9)); s.listen(64)
while True:
    c,_=s.accept(); c.sendall(b"hello\n"); c.close()' &
  echo $! > "$T/echo.pid"
  sleep 0.5
}

write_configs() {
  mkdir -p "$T/relay" "$T/client"
  KEY=$("$BIN" version >/dev/null; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  echo "$KEY" > "$T/relay/psk"; echo "$KEY" > "$T/client/psk"; chmod 600 "$T"/*/psk
  cat > "$T/relay/tandem.json" <<J
{"role":"relay","listen":"192.168.78.2:7411","allow":["192.168.77.0/24"],"run_dir":"$T/relay/run","state_dir":"$T/relay/state"}
J
  cat > "$T/client/tandem.json" <<J
{"role":"client","relay":"192.168.78.2:7411","run_dir":"$T/client/run","state_dir":"$T/client/state"}
J
}

start_relay()  { TANDEM_CONFIG=$T/relay/tandem.json ip netns exec t-relay "$BIN" run >>"$T/relay.log" 2>&1 & echo $! > "$T/relay.pid"; }
stop_pid() { local p; p=$(cat "$T/$1.pid"); kill -9 "$p" 2>/dev/null; while kill -0 "$p" 2>/dev/null; do sleep 0.05; done; }
start_client() { TANDEM_DEBUG=1 TANDEM_CONFIG=$T/client/tandem.json ip netns exec t-client "$BIN" run >>"$T/client.log" 2>&1 & echo $! > "$T/client.pid"; }
cli() { TANDEM_CONFIG=$T/client/tandem.json "$BIN" "$@"; }
rules_present() { inc nft list table ip tandem 2>/dev/null | grep -q 'redirect to'; }
table_present() { inc nft list table ip tandem >/dev/null 2>&1; }
wait_rules() { # $1 = want (1 present / 0 absent), $2 = timeout s
  local i; for i in $(seq 1 $(( $2 * 10 ))); do
    if [ "$1" = 1 ] && rules_present; then return 0; fi
    if [ "$1" = 0 ] && ! rules_present; then return 0; fi
    sleep 0.1; done; return 1; }
speed() { # prints Mbit/s
  inc curl -s -o /dev/null --max-time 120 -w '%{speed_download}' http://203.0.113.10/big.bin | awk '{printf "%.0f", $1*8/1e6}'; }
ms() { date +%s%3N; }

setup_net; start_inet; write_configs
echo "== shaping: wifi delay $WIFI_DELAY loss $WIFI_LOSS each way, WAN delay $WAN_DELAY each way, file ${SIZE_MB} MiB"

if [ -z "${FAST:-}" ]; then d1=$(speed); d2=$(speed); else d1=-; d2=-; fi
echo "direct (no tandem): $d1 / $d2 Mbit/s"

start_relay; start_client
if wait_rules 1 10; then ok "rules installed after relay became healthy"; else bad "rules never installed"; cat "$T/client.log"; fi
if [ -z "${FAST:-}" ]; then r1=$(speed); r2=$(speed); else r1=-; r2=-; fi
echo "through tandem: $r1 / $r2 Mbit/s"
cli status | sed -n 1,6p

# Echo service sanity: short connection through the proxy returns the right bytes.
out=$(inc curl -s --max-time 5 telnet://203.0.113.10:9 </dev/null)
[ "$out" = "hello" ] && ok "short connection content intact via tandem" || bad "short connection got '$out'"
inc curl -s -o /dev/null http://203.0.113.10/; inc curl -s -o /dev/null http://203.0.113.10/; inc curl -s -o /dev/null http://203.0.113.10/
cli status --json | grep -q '"relay_total":[1-9]' && ok "connections went through the relay" || bad "no relayed connections"

# private destinations are never redirected
inc nft list table ip tandem | grep -q 'ip daddr 192.168.0.0/16 return' && ok "LAN bypass rule present" || bad "LAN bypass missing"

# ---- force pins the path
cli force direct >/dev/null; inc curl -s -o /dev/null http://203.0.113.10/
cli status --json | python3 -c 'import json,sys; r=json.load(sys.stdin)["recent"][-1]; sys.exit(0 if r["path"]=="direct" and "forced" in r["reason"] else 1)' && ok "force direct pins new connections" || bad "force direct"
cli force relay >/dev/null; inc curl -s -o /dev/null http://203.0.113.10/
cli status --json | python3 -c 'import json,sys; r=json.load(sys.stdin)["recent"][-1]; sys.exit(0 if r["path"]=="relay" else 1)' && ok "force relay pins new connections" || bad "force relay"
cli force auto >/dev/null

# ---- kill -9 the client: rules must vanish with it
kill -9 "$(cat "$T/client.pid")"; t0=$(ms)
for i in $(seq 20); do table_present || break; sleep 0.1; done
if ! table_present; then ok "kill -9: kernel dropped the whole table in $(( $(ms)-t0 )) ms"; else bad "kill -9 left the table behind"; fi
s=$(inc curl -s -o /dev/null --max-time 10 -w '%{http_code}' http://203.0.113.10/big.bin)
[ "$s" = 200 ] && ok "traffic flows direct after daemon death" || bad "traffic broken after daemon death ($s)"

# ---- SIGSTOP (hung daemon, no systemd watchdog here): document the behaviour
start_client; wait_rules 1 10
# ---- relay asleep: silently drop everything at the relay
ip netns exec t-relay nft -f - <<N
table inet sleep {
  chain in { type filter hook input priority -300; policy drop; }
}
N
t0=$(ms)
s=$(inc curl -s -o /dev/null --max-time 10 -w '%{http_code}' http://203.0.113.10/)
dt=$(( $(ms)-t0 ))
[ "$s" = 200 ] && [ $dt -lt 1500 ] && ok "relay asleep: first connection went direct in $dt ms" || bad "relay asleep: http=$s after $dt ms"
for i in 2 3 4; do t1=$(ms); inc curl -s -o /dev/null --max-time 10 http://203.0.113.10/; echo "   request $i while relay asleep: $(( $(ms)-t1 )) ms"; done
t0=$(ms)
if wait_rules 0 5; then ok "relay asleep: rules removed $(( $(ms)-t0 )) ms later"; else bad "rules stayed while relay asleep"; fi
s=$(inc curl -s -o /dev/null --max-time 10 -w '%{http_code}' http://203.0.113.10/)
[ "$s" = 200 ] && ok "traffic fine while relay asleep" || bad "traffic broken while relay asleep"
ip netns exec t-relay nft delete table inet sleep
t0=$(ms)
if wait_rules 1 10; then ok "relay woke: rules back after $(( $(ms)-t0 )) ms"; else bad "rules did not return"; fi

# ---- relay process dead (RST instead of silence)
stop_pid relay
t0=$(ms)
s=$(inc curl -s -o /dev/null --max-time 10 -w '%{http_code}' http://203.0.113.10/)
dt=$(( $(ms)-t0 ))
[ "$s" = 200 ] && ok "relay dead: connection went direct in $dt ms" || { bad "relay dead: http=$s after $dt ms"; tail -5 "$T/client.log"; }
wait_rules 0 5 && ok "relay dead: redirect paused $(( $(ms)-t0 )) ms after the relay died" || bad "relay dead: rules stayed"
for i in 1 2 3; do t1=$(ms); inc curl -s -o /dev/null --max-time 10 http://203.0.113.10/; echo "   request $i after relay death: $(( $(ms)-t1 )) ms"; done
start_relay; wait_rules 1 10

# ---- another VPN takes the default route
inc ip tuntap add vpn0 mode tun; inc ip link set vpn0 up
inc ip route add 0.0.0.0/1 dev vpn0; inc ip route add 128.0.0.0/1 dev vpn0
if wait_rules 0 5; then ok "VPN default route detected: rules removed"; else bad "VPN not detected"; fi
cli status | grep -q "another VPN" && ok "status explains the VPN" || bad "status does not mention VPN"
inc ip link del vpn0
wait_rules 1 10 && ok "VPN gone: rules back" || bad "rules did not return after VPN"

# ---- a transfer in flight survives the redirect being paused
inc curl -s -o "$T/inflight.bin" --limit-rate 2M --max-time 60 http://203.0.113.10/big.bin & cp=$!
sleep 1; cli disable >/dev/null; sleep 1; cli enable >/dev/null
if wait $cp && cmp -s "$T/inflight.bin" "$T/www/big.bin"; then ok "in-flight transfer survived disable/enable intact"; else bad "in-flight transfer broke on pause"; fi
wait_rules 1 6

# ---- disable / enable
cli disable --dry-run | grep -q dry-run && rules_present && ok "disable --dry-run changes nothing" || bad "dry-run"
cli disable >/dev/null; wait_rules 0 3 && ok "tandem disable removes rules" || bad "disable"
stop_pid client; start_client; sleep 4
rules_present && bad "disable did not persist across restart" || ok "disable persists across daemon restart"
cli enable >/dev/null; wait_rules 1 6 && ok "tandem enable restores rules" || bad "enable"

# ---- why
cli why 203.0.113.10 --port 80 | head -3
cli why 192.168.1.1 | grep -qi "never relayed" && ok "why: private address explained" || bad "why private"

# ---- relay refuses: wrong key, private destination, foreign source
echo 00000000000000000000000000000000000000000000000000000000000000ff > "$T/bad.psk"
cat > "$T/bad.json" <<J
{"role":"client","relay":"192.168.78.2:7411","psk_file":"$T/bad.psk","run_dir":"$T/badrun","state_dir":"$T/badstate"}
J
TANDEM_CONFIG=$T/relay/tandem.json ip netns exec t-relay true
before=$(TANDEM_CONFIG=$T/relay/tandem.json "$BIN" status --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["auth_failures"])')
inc python3 - "$T" <<'P'
import socket,sys,os
s=socket.create_connection(("192.168.78.2",7411),timeout=2)
s.sendall(b"TDM1"+os.urandom(8+16)+b"\x00"+os.urandom(32))
try: print("reply", s.recv(64))
except Exception as e: print("closed", e)
P
after=$(TANDEM_CONFIG=$T/relay/tandem.json "$BIN" status --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["auth_failures"])')
[ "$after" -gt "$before" ] && ok "relay rejects an unauthenticated request" || bad "auth failure not counted"
# source not in allow list: the router itself (192.168.78.1)
r=$(ip netns exec t-router python3 -c '
import socket
s=socket.create_connection(("192.168.78.2",7411),timeout=2)
s.sendall(b"x"*64)
try: print(len(s.recv(10)))
except Exception as e: print("reset")')
[ "$r" = "reset" ] || [ "$r" = "0" ] && ok "relay drops clients outside the allow list" || bad "foreign source got '$r'"

# ---- second, learned-state check: stats persist across restart
kill "$(cat "$T/client.pid")"; sleep 1
[ -s "$T/client/state/stats.json" ] && ok "learned stats saved on stop" || bad "no stats file"
table_present && bad "SIGTERM left the table" || ok "SIGTERM removed the table"

echo
echo "summary: direct $d1/$d2 Mbit/s, tandem $r1/$r2 Mbit/s"
echo "PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
