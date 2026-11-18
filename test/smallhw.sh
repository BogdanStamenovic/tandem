#!/usr/bin/env bash
# What the relay costs, and how it does on a small single-core box
# (README "Small hardware"). Needs root. Network namespaces on one machine:
#
#   t-client 192.168.77.3 --\
#   t-relay  192.168.78.2 ---- t-router (the home switch/router)
#   t-inet   203.0.113.10 --/   (WAN leg: netem delay)
#
# The relay has one link, shaped with tbf to LINK each way, so relayed bytes
# cross it twice (in from the internet, out to the client), as on a board
# with one Ethernet port.
#
# Two ways to run the relay:
#   native  `relay` + `measure`: on RELAY_CPU, with RPS putting its veth's
#           receive softirq there too. Caveat: veth runs the receiver's work
#           on the sender's CPU, so that CPU also pays for the emulated
#           router and the shaper. Process-only time is printed beside it.
#   VM      `vm PERIOD` + `vmmeasure`: a 1-vCPU, VM_MEM KVM guest whose vCPU
#           gets 1 ms per PERIOD µs. 13000 = Cortex-A7 @1.2 GHz by CoreMark
#           (2,447/s measured under that cap). The cap covers the guest
#           kernel's network stack, which a cgroup on a host process cannot.
#
#   sudo test/smallhw.sh up
#   sudo test/smallhw.sh vm 13000
#   sudo LINK=100mbit test/smallhw.sh shape
#   sudo test/smallhw.sh vmmeasure LABEL down|up|rr|conn CONNS SECS
#   sudo test/smallhw.sh vmcap [PERIOD]            # change the cap live
#   sudo test/smallhw.sh relay                     # native relay instead
#   sudo test/smallhw.sh measure LABEL via|direct MODE CONNS SECS
#   sudo test/smallhw.sh down                      # also restores kvm halt_poll_ns
#
# Switching from VM to native: stop the VM, `ip -n t-relay addr add
# 192.168.78.2/24 dev br0`, add the default route, flush the router's neighbours.
# Env: LINK (100mbit|1gbit|none), OFFLOAD (off|on), WAN_DELAY, RELAY_CPU, NIC_CPU, VM_MEM.
set -uo pipefail
HERE=$(dirname "$(readlink -f "$0")")
BIN=${BIN:-$HERE/../bin/tandem}
BENCH=${BENCH:-$HERE/../bin/relaybench}
T=/run/tandem-smallhw
LINK=${LINK:-1gbit}
OFFLOAD=${OFFLOAD:-off}
WAN_DELAY=${WAN_DELAY:-5ms}
RELAY_CPU=${RELAY_CPU:-5}
NIC_CPU=${NIC_CPU:-4}
OTHER_CPUS=${OTHER_CPUS:-0-3}
NS="t-client t-relay t-inet t-router"

up() {
  mkdir -p "$T"
  for n in $NS; do ip netns add $n; ip -n $n link set lo up; done
  ip link add c0 netns t-client type veth peer name rc netns t-router
  ip link add s0 netns t-relay  type veth peer name rs netns t-router
  ip link add i0 netns t-inet   type veth peer name ri netns t-router
  ip -n t-client addr add 192.168.77.3/24 dev c0; ip -n t-router addr add 192.168.77.1/24 dev rc
  ip -n t-relay  addr add 192.168.78.2/24 dev s0; ip -n t-router addr add 192.168.78.1/24 dev rs
  ip -n t-inet   addr add 203.0.113.10/24 dev i0; ip -n t-router addr add 203.0.113.1/24 dev ri
  for l in "t-client c0" "t-relay s0" "t-inet i0" "t-router rc" "t-router rs" "t-router ri"; do
    set -- $l; ip -n $1 link set $2 up
    # Without this veth hands over 64 KiB GSO/GRO super-packets, and per-packet
    # kernel cost (the part that dominates on a small CPU) almost disappears.
    [ "$OFFLOAD" = off ] && ip netns exec $1 ethtool -K $2 tso off gso off gro off >/dev/null 2>&1
  done
  ip -n t-client route add default via 192.168.77.1
  ip -n t-relay  route add default via 192.168.78.1
  ip -n t-inet   route add default via 203.0.113.1
  ip netns exec t-router sysctl -qw net.ipv4.ip_forward=1
  tc -n t-inet   qdisc add dev i0 root netem delay $WAN_DELAY limit 100000
  tc -n t-router qdisc add dev ri root netem delay $WAN_DELAY limit 100000
  shape
  # Receive processing for the relay's link on the relay's CPU.
  printf '%x' $((1 << RELAY_CPU)) | ip netns exec t-relay tee /sys/class/net/s0/queues/rx-0/rps_cpus >/dev/null 2>&1 \
    || ip netns exec t-relay sh -c "printf '%x' $((1 << RELAY_CPU)) > /sys/class/net/s0/queues/rx-0/rps_cpus"
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$T/psk"; chmod 600 "$T/psk"
  cat > "$T/relay.json" <<J
{"role":"relay","listen":"192.168.78.2:7411","allow":["192.168.77.0/24"],"run_dir":"$T/run","state_dir":"$T/state"}
J
  ip netns exec t-inet taskset -c $OTHER_CPUS "$BENCH" serve -addr 203.0.113.10:5001 >/dev/null 2>&1 &
  echo $! > "$T/serve.pid"
  echo "lab up: LINK=$LINK OFFLOAD=$OFFLOAD WAN_DELAY=$WAN_DELAY"
}

shape() {
  for d in "t-relay s0" "t-router rs"; do
    set -- $d
    tc -n $1 qdisc del dev $2 root 2>/dev/null
    [ "$LINK" = none ] && continue
    tc -n $1 qdisc add dev $2 root tbf rate $LINK burst 64kb latency 20ms
  done
}

relay() { # [CPUQUOTA% MEMMAX]
  stop_relay
  local props=(-p AllowedCPUs=$RELAY_CPU)
  [ -n "${1:-}" ] && props+=(-p CPUQuota=$1)
  [ -n "${2:-}" ] && props+=(-p MemoryMax=$2 -p MemorySwapMax=0)
  systemd-run --quiet --unit=tandem-smallhw-relay --collect "${props[@]}" \
    -E TANDEM_CONFIG=$T/relay.json -E GOMAXPROCS=1 \
    ip netns exec t-relay "$BIN" run
  local i; for i in $(seq 50); do
    ip netns exec t-relay ss -ltn 2>/dev/null | grep -q ':7411' && return 0; sleep 0.1; done
  echo "relay did not start"; journalctl -u tandem-smallhw-relay -n 20 --no-pager; return 1
}
stop_relay() { systemctl stop tandem-smallhw-relay 2>/dev/null; systemctl reset-failed tandem-smallhw-relay 2>/dev/null; true; }
relay_pid() { systemctl show -p MainPID --value tandem-smallhw-relay; }

cpu_busy() { # jiffies busy on cpu $1 (or "cpu" for all)
  awk -v c="$1" '$1==c {print $2+$3+$4+$7+$8+$9}' /proc/stat; }
proc_cpu() { awk '{print $14+$15}' /proc/$1/stat 2>/dev/null || echo 0; }

measure() { # LABEL via|direct MODE CONNS SECS
  local label=$1 via=$2 mode=$3 conns=$4 secs=$5 pid="" args=()
  if [ "$via" = via ]; then pid=$(relay_pid); args=(-relay 192.168.78.2:7411 -psk "$T/psk"); fi
  local c0 a0 p0=0 c1 a1 p1=0 hz; hz=$(getconf CLK_TCK)
  c0=$(cpu_busy cpu$RELAY_CPU); a0=$(cpu_busy cpu)
  [ -n "$pid" ] && p0=$(proc_cpu $pid)
  ( sleep $((secs/2)); if [ -n "$pid" ]; then
      echo "mid: threads=$(ls /proc/$pid/task | wc -l) fds=$(ls /proc/$pid/fd | wc -l) rss_kb=$(awk '/VmRSS/{print $2}' /proc/$pid/status) mhz=$(awk -v c=$RELAY_CPU '$1=="processor"&&$3==c{f=1} f&&/cpu MHz/{print int($4);exit}' /proc/cpuinfo)" > "$T/mid"
    fi ) &
  local out; out=$(ip netns exec t-client taskset -c $OTHER_CPUS "$BENCH" run "${args[@]}" -dest 203.0.113.10:5001 -mode $mode -conns $conns -dur ${secs}s)
  wait
  c1=$(cpu_busy cpu$RELAY_CPU); a1=$(cpu_busy cpu)
  [ -n "$pid" ] && p1=$(proc_cpu $pid)
  local hwm=""; [ -n "$pid" ] && hwm=$(awk '/VmHWM/{print $2}' /proc/$pid/status)
  python3 - "$label" "$out" $(( c1-c0 )) $(( a1-a0 )) $(( p1-p0 )) $hz "$secs" "${hwm:-0}" "$(cat "$T/mid" 2>/dev/null)" <<'PY'
import json, sys
label, out, cpuj, allj, procj, hz, secs, hwm, mid = sys.argv[1:]
r = json.loads(out); hz = int(hz); secs = float(r["seconds"])
cpu = int(cpuj)/hz; allc = int(allj)/hz; proc = int(procj)/hz
line = {"label": label, **r, "relay_cpu_util": round(cpu/secs, 3), "relay_proc_util": round(proc/secs, 3),
        "all_cpu_s": round(allc, 2), "vm_hwm_kb": int(hwm), "mid": mid}
if r.get("bytes"):
    line["relay_cpu_ns_per_byte"] = round(cpu*1e9/r["bytes"], 3)
    line["proc_ns_per_byte"] = round(proc*1e9/r["bytes"], 3)
print(json.dumps(line))
PY
  rm -f "$T/mid"
}

# ---- VM mode: the relay runs inside a 1-vCPU KVM guest with VM_MEM of RAM,
# bridged onto the relay's link. Only the vCPU thread is capped (cpu.max in a
# threaded sub-cgroup); QEMU's vhost and main-loop threads stand in for the
# NIC's DMA and sit uncapped next to it. The guest kernel's network stack runs
# on the vCPU, so the cap covers it, which a cgroup on a host process cannot.
VM_UNIT=tandem-smallhw-vm
VM_MEM=${VM_MEM:-256M}
CG=/sys/fs/cgroup/system.slice/$VM_UNIT.service

vm_build() {
  local r=$T/initrd.d; rm -rf "$r"; mkdir -p "$r"/{proc,sys,dev,run,etc,lib}
  ( cd "$HERE/.." && CGO_ENABLED=0 go build -o "$r/tandem" ./cmd/tandem && CGO_ENABLED=0 go build -o "$r/init" ./test/vminit ) || return 1
  local m=/lib/modules/$(uname -r)/kernel
  for k in net/core/failover drivers/net/net_failover drivers/net/virtio_net; do
    zstd -qdc "$m/$k.ko.zst" > "$r/lib/$(basename $k).ko"; done
  cp "$T/psk" "$r/etc/psk"
  echo '{"role":"relay","listen":"192.168.78.2:7411","allow":["192.168.77.0/24"],"psk_file":"/etc/psk","run_dir":"/run/tandem","state_dir":"/run/state"}' > "$r/etc/tandem.json"
  ( cd "$r" && find . | cpio -o -H newc --quiet ) > "$T/initrd"
}

vm() { # [PERIOD_US: vCPU gets 1 ms per PERIOD; 13000 = A7 @1.2 GHz]
  stop_vm; stop_relay
  [ -f "$T/initrd" ] || vm_build || return 1
  if ! ip -n t-relay link show br0 >/dev/null 2>&1; then
    ip -n t-relay addr flush dev s0
    ip -n t-relay link add br0 type bridge; ip -n t-relay link set br0 up
    ip -n t-relay link set s0 master br0
    ip -n t-relay tuntap add tap0 mode tap; ip -n t-relay link set tap0 master br0; ip -n t-relay link set tap0 up
    [ "$OFFLOAD" = off ] && ip netns exec t-relay ethtool -K tap0 tso off gso off gro off >/dev/null 2>&1
  fi
  # RV1106's MAC has checksum offload but no TSO: no TSO/GSO in either direction.
  local nic=virtio-net-pci,netdev=n0,host_tso4=off,host_tso6=off,host_ecn=off,host_ufo=off,guest_tso4=off,guest_tso6=off,guest_ecn=off,guest_ufo=off
  # KVM halt polling spins the vCPU thread while the guest idles; that would be
  # charged against the cap. It is a module-wide knob, restored by `down`.
  [ -f "$T/halt_poll_ns" ] || cat /sys/module/kvm/parameters/halt_poll_ns > "$T/halt_poll_ns"
  echo 0 > /sys/module/kvm/parameters/halt_poll_ns
  systemd-run --quiet --unit=$VM_UNIT --collect -p Delegate=yes -p AllowedCPUs=$RELAY_CPU,$NIC_CPU \
    ip netns exec t-relay qemu-system-x86_64 -enable-kvm -cpu host -smp 1 -m $VM_MEM -nodefaults -no-reboot \
      -kernel /boot/vmlinuz-linux -initrd "$T/initrd" \
      -append "console=ttyS0 quiet panic=-1 tandem.ip=192.168.78.2/24 tandem.gw=192.168.78.1" \
      -display none -serial file:"$T/console.log" -monitor none \
      -netdev tap,id=n0,ifname=tap0,script=no,downscript=no,vhost=on -device $nic
  local i; for i in $(seq 100); do
    ip netns exec t-client bash -c 'exec 3<>/dev/tcp/192.168.78.2/7411' 2>/dev/null && break; sleep 0.2; done
  [ $i = 100 ] && { echo "VM relay never came up"; tail -20 "$T/console.log"; return 1; }
  vm_cap "${1:-}"
  echo "vm up after ~$((i*2/10))s, vcpu cpu.max: $(cat $CG/vcpu/cpu.max)"
}

vm_cap() { # put the vCPU thread alone in a capped threaded cgroup
  local pid tid; pid=$(systemctl show -p MainPID --value $VM_UNIT)
  [ -d $CG/vcpu ] || { mkdir $CG/rest $CG/vcpu; echo threaded > $CG/rest/cgroup.type; echo threaded > $CG/vcpu/cgroup.type
    for tid in $(ls /proc/$pid/task); do echo $tid > $CG/rest/cgroup.threads 2>/dev/null; done
    echo +cpu > $CG/cgroup.subtree_control; }
  # The vCPU gets RELAY_CPU to itself; QEMU's other threads (vhost = the NIC)
  # run on NIC_CPU, so they neither steal from nor count against the guest.
  for tid in $(ls /proc/$pid/task); do
    if grep -q 'CPU 0/KVM' /proc/$pid/task/$tid/comm; then
      echo $tid > $CG/vcpu/cgroup.threads; taskset -pc $RELAY_CPU $tid >/dev/null
    else taskset -pc $NIC_CPU $tid >/dev/null; fi; done
  # 1 ms of CPU per PERIOD µs (1 ms is the smallest quota the kernel takes;
  # a short period keeps throttling stalls at most PERIOD-1 ms).
  if [ -n "${1:-}" ]; then echo "1000 $1" > $CG/vcpu/cpu.max; else echo "max 100000" > $CG/vcpu/cpu.max; fi
}
stop_vm() { systemctl stop $VM_UNIT 2>/dev/null; systemctl reset-failed $VM_UNIT 2>/dev/null; true; }

guest_stat() { ip netns exec t-client bash -c 'exec 3<>/dev/tcp/192.168.78.2/9100; cat <&3'; }

vmmeasure() { # LABEL MODE CONNS SECS
  local label=$1 mode=$2 conns=$3 secs=$4
  local g0 g1 v0 v1 r0 r1 out
  g0=$(guest_stat); v0=$(awk '/usage_usec/{print $2}' $CG/vcpu/cpu.stat); r0=$(awk '/usage_usec/{print $2}' $CG/rest/cpu.stat)
  out=$(ip netns exec t-client taskset -c $OTHER_CPUS "$BENCH" run -relay 192.168.78.2:7411 -psk "$T/psk" -dest 203.0.113.10:5001 -mode $mode -conns $conns -dur ${secs}s)
  g1=$(guest_stat); v1=$(awk '/usage_usec/{print $2}' $CG/vcpu/cpu.stat); r1=$(awk '/usage_usec/{print $2}' $CG/rest/cpu.stat)
  python3 - "$label" "$out" "$g0" "$g1" $((v1-v0)) $((r1-r0)) "$(cat $CG/vcpu/cpu.max)" <<'PY2'
import json, sys
label, out, g0, g1, vus, rus, cap = sys.argv[1:]
r = json.loads(out); secs = float(r["seconds"])
def parse(g):
    d = {}
    for l in g.splitlines():
        f = l.split()
        if f and f[0] == "cpu":
            v = list(map(int, f[1:])); d["busy"] = v[0]+v[1]+v[2]+v[5]+v[6]; d["idle"] = v[3]+v[4]; d["steal"] = v[7]
        elif f and f[0].endswith(":"):
            d[f[0][:-1]] = int(f[1])
    return d
a, b = parse(g0), parse(g1)
busy = (b["busy"]-a["busy"]); steal = b["steal"]-a["steal"]; tot = busy + (b["idle"]-a["idle"]) + steal
line = {"label": label, **r, "cap": cap, "guest_busy": round(busy/tot, 3) if tot else None, "guest_steal": round(steal/tot, 3) if tot else None,
        "vcpu_host_util": round(int(vus)/1e6/secs, 3), "qemu_rest_util": round(int(rus)/1e6/secs, 3),
        "guest_mem_avail_kb": b.get("MemAvailable"), "relay_rss_kb": b.get("VmRSS"), "relay_hwm_kb": b.get("VmHWM"),
        "relay_threads": b.get("Threads"), "relay_fds": b.get("FDs")}
if r.get("bytes"):
    line["vcpu_ns_per_byte"] = round(int(vus)*1e3/r["bytes"], 3)
print(json.dumps(line))
PY2
}

down() {
  stop_vm
  [ -f "$T/halt_poll_ns" ] && cat "$T/halt_poll_ns" > /sys/module/kvm/parameters/halt_poll_ns
  stop_relay
  [ -f "$T/serve.pid" ] && kill "$(cat "$T/serve.pid")" 2>/dev/null
  for n in $NS; do ip netns del $n 2>/dev/null; done
  rm -rf "$T"; echo "lab down"
}

cmd=${1:-}; shift || true
case "$cmd" in
  up) up ;; down) down ;; vm) vm "$@" ;; vmcap) vm_cap "$@" ;; vmmeasure) vmmeasure "$@" ;; vmbuild) vm_build ;; relay) relay "$@" ;; shape) shape ;; measure) measure "$@" ;;
  *) sed -n 2,34p "$0"; exit 2 ;;
esac
