# tandem

tandem is a systemd service that transparently relays a laptop's outbound TCP connections through a faster machine on the same LAN, and keeps doing so per destination only while it measures faster.

The case it was built for: a laptop on 5 GHz Wi-Fi at -69 dBm and a desktop (`archserver`) on gigabit Ethernet, behind the same router. Long-RTT downloads on the laptop are limited by Wi-Fi packet loss, because TCP's congestion control treats every lost Wi-Fi frame as internet congestion. Cutting each connection in two (laptop to archserver over the LAN, archserver to the internet over Ethernet) lets each leg recover from its own losses at its own RTT. That is split TCP. No application changes, no proxy settings: `yay -Syu`, `git clone`, a browser, anything TCP.

## Why it helps (measured before building)

Prototype measurement on 2026-11-18 (laptop over Wi-Fi, a throwaway asyncio CONNECT proxy on archserver, Mbit/s, two rounds each):

| Destination | Direct | Through archserver |
|---|---|---|
| Hetzner fsn1, 1 stream | 39 / 67 | 189 / 147 |
| Hetzner fsn1, 4 streams | 72 / 93 | 189 / 132 |
| Cloudflare | 133 / 167 | 208 / 42 (noisy) |
| Arch mirror package (Fastly) | 197 / 180 | 232 / 221 |

For reference, archserver directly reached Cloudflare at ~400-500 and the mirror at 347, and the LAN path archserver to laptop at ~212-242. So the relay can help most where the laptop's direct path is worst (far, high-RTT servers), helps little on close CDNs, and can never beat the ~220 Mbit/s Wi-Fi leg. The Cloudflare 42 shows a relayed connection can also lose, which is why tandem measures per destination instead of relaying everything.

`test/bench.sh` on archserver itself, 2026-11-18 ~03:00 UTC, as the ceiling the relay can offer (Mbit/s): Hetzner fsn1 187-212, OVH 361-374, Hetzner Ashburn 54-57, `extra.files.tar.gz` from the Fastly mirror 274-279, `pacman -Sw python go` 217-243.

## Measured on the real laptop

`test/bench.sh 3` on the laptop with tandem installed, 2026-11-18 10:30-10:36 UTC. Wi-Fi was at -64 dBm, 720 Mbit/s PHY rate. Each round pins every new connection to direct and then to relay (`tandem force`), in alternating order. Mbit/s for one 100 MB (or ~50 MB mirror file) download per cell; pacman is the total bytes of `pacman -Sw linux go python` over wall time, with pacman's parallel downloads.

| Target | Direct (3 rounds) | Relay (3 rounds) | Median direct to relay |
|---|---|---|---|
| Hetzner fsn1 (DE) | 50 / 51 / 51 | 83 / 132 / 59 | 51 to 83 (+63 %) |
| OVH (FR) | 145 / 178 / 180 | 137 / 137 / 212 | 178 to 137 (-23 %) |
| Hetzner Ashburn (US) | 36 / 47 / 37 | 45 / 55 / 44 | 37 to 45 (+22 %) |
| Fastly mirror, `extra.files.tar.gz` | 159 / 188 / 59 | 139 / 191 / 201 | 159 to 191 (+20 %) |
| `pacman -Sw linux go python` | 135 / 166 / 156 | 178 / 37 / 175 | 156 to 175 (+12 %) |

What that says, honestly:

- The laptop's Wi-Fi was in much better shape than during the prototype (direct Hetzner 50 now against 39-67 then, mirror ~160-190 against 180-197), so there was less loss for split TCP to hide. The big prototype gap (39 to 189) did not reappear today.
- The clear win is the one destination where the direct path was worst: Hetzner fsn1, relayed better in all 3 rounds. Ashburn was better in all 3 too, but by less; it is capped by archserver's own 54-57.
- OVH was faster direct in 2 of 3 rounds. That is exactly the case the per-destination engine exists for. After the bench it had learned relay 164 against direct 167 for OVH's /24, a tie, so the default (relay) stays; the margin rule needs a 10 % difference to switch.
- Two single cells are outliers (relay pacman 37, direct mirror 59). Three rounds on Wi-Fi are not enough to call differences under ~20 %.
- What the engine had learned overall after the run: relay 104 Mbit/s (n=22), direct 85 Mbit/s (n=21).

A second run, by a different agent (manager-17), right after the bench above: direct vs tandem vs the original prototype proxy (`research/prototype_connect_proxy.py` on archserver, used via `curl -x`), one single-stream curl per cell, Mbit/s, 3 rounds. The mirror target was `core/linux-firmware-nvidia-20261019-1-any.pkg.tar.zst` from the Fastly mirror.

| Round | Hetzner fsn1: direct / tandem / prototype | Fastly mirror: direct / tandem / prototype |
|---|---|---|
| 1 | 53 / 130 / 123 | 195 / 219 / 237 |
| 2 | 56 / 126 / 109 | 140 / 237 / 207 |
| 3 | 60 / 128 / 172 | 189 / 244 / 211 |

tandem landed where the prototype did, so the transparent redirect and the authenticated wire protocol cost nothing measurable here. Its Hetzner numbers in this run (126-130) are higher and steadier than in the bench (59-132); that spread is the Wi-Fi, not the relay.

## How it works

```
laptop app ──TCP──▶ [nft redirect] ──▶ tandem client (127.0.0.1:7410)
                                         │ SO_ORIGINAL_DST = real destination
                                         ├── direct: dial it (socket marked, not redirected again)
                                         └── relay:  dial archserver:7411, authenticated request
                                                      └── tandem relay dials the real destination
```

**Client (laptop, root service).**

- An nftables `nat` output chain redirects new outbound IPv4 TCP connections to the local daemon. The chain skips:
  - private, loopback, link-local, CGNAT and tailnet addresses;
  - Moonlight's TCP ports;
  - tailscaled's own marked sockets;
  - the daemon's own marked sockets;
  - anything not leaving through the physical uplink.
- The daemon reads the original destination (`SO_ORIGINAL_DST`) and picks a path per connection.
- Learned statistics persist in `/var/lib/tandem/stats.json`.

**Relay (archserver, unprivileged service).**

- It listens on `0.0.0.0:7411`, so a new DHCP address changes nothing. It accepts a source only if it is in the allow list *and* proves the pre-shared key (HMAC-SHA256 over a timestamp, a nonce and the destination, with replay protection).
- The reply is authenticated too, so a LAN impostor cannot pose as the relay.
- It refuses private, loopback and tailnet destinations, so it cannot be used to reach archserver's own services or its LAN.
- It is not an open proxy.
- It does not encrypt: the payload is whatever the application sent, usually TLS already.

**Discovery** (`internal/discovery`, `internal/client/discover.go`): the client is not told where the relay is; it finds it.

- **Beacons.** Every 5 s the relay sends a signed beacon (UDP 7411) to the directed broadcast of each of its addresses that lies inside its allow list. The beacon carries the relay's own address, a timestamp and a nonce, and an HMAC under a key derived from the pairing key (never the key itself). The client uses the beacon's source address, and only if it equals the signed address.
- **Queries.** When the client starts, when the network changes, and when a probe of the current relay fails, it broadcasts a signed query. The relay answers by unicast with the same signed format, echoing the query's nonce. A move of the relay to a new address is noticed within one round trip of the first failed probe, without waiting for the next beacon.
- **Rejected:** a wrong key (`bad_mac`), a timestamp outside ±120 s (`stale`), a nonce already seen (`replay`), a packet whose source differs from the signed address (`source_mismatch`, which makes a captured beacon useless from any other host), an answer to a query this client never sent, a non-private or off-subnet address. `tandem status` counts each.
- **Known networks.** A network is identified by its default gateway's MAC address plus the client's subnet. When the relay answers an authenticated handshake on a network (or `tandem pair`, `setup` or `upgrade` gets an answer from the static address there), that network is recorded as home in `/var/lib/tandem/discovery.json`, with the address that worked.
- **Fallbacks, only on a known home network**, for when broadcast is filtered (Wi-Fi client isolation): first the last address that worked there (`cached`), then `relay` from the config (`static`). Two failed probes of one move to the other.
- **Anywhere else, nothing to any relay address.** On a network that is not a known home and where no valid beacon is heard, the client has no relay address: no probe, no ARP, no handshake goes to the old relay IP or anyone else. What it does send there is the signed query, to the subnet broadcast address, when it joins the network and on changes (60 bytes; it reveals that a tandem client is present, not the key). The redirect stays paused.
- **Network changes are acted on at once.** A netlink subscription (link, IPv4 address, IPv4 route) suspends probes and relay dials the moment anything changes, until the network is identified again. Only an unchanged network gets its relay back.
- `tandem status` shows the address, where it came from (`beacon`, `query`, `cached`, `static`) and the counters. `"discovery": false` in the client config restores the old behaviour: the static address, used everywhere.

Measured in `test/netns.sh` (client on a 3 ms, 1 %-loss "Wi-Fi" leg; the last two clean runs, 47/47 each):

| Event | Result |
|---|---|
| Client starts with no relay address configured | rules up after 1.07 s (query answered); one earlier run lost the query and took 3.4 s, until the next beacon |
| Relay's address changes (.2 to .20) | relaying again via the new address in 2.0-2.4 s (query after the first failed probe; 5.4 s when it waited for a beacon) |
| Beacon signed with another key | rejected (`bad_mac`), relay unchanged |
| Captured beacon replayed from another host / from the relay's address | `source_mismatch` / `replay` |
| Broadcast filtered, known home | up via `cached` in 1.1 s |
| Fresh state, broadcast filtered, no static address | stays paused; `tandem pair` records the network once the relay answers, then up via `cached` in 1.1 s |
| Cached address dead | `static` in 3.1-3.2 s |
| Moved to a foreign network whose hosts hold .1, .2 and .20 | relay address dropped 52-54 ms after the move; **0 packets** (IP or ARP) to the relay addresses or TCP 7411 in 15 s, counted by nft in the foreign namespace |
| Back home | relaying again in 1.1 s |

**The decision loop** (`internal/decide`, independent of sockets and nftables):

- Health: an authenticated ping to the relay every 2 s (1 s while down). Two successes mark it up; two failures, or a probe slower than 200 ms, mark it down. A real connection failing to reach the relay counts as a failure immediately.
- Throughput: each finished connection that moved at least 1 MiB in its busier direction, over at least 300 ms of active transfer, is a sample. Gaps longer than 250 ms are not counted, so an idle application does not make a path look slow.
- Samples feed an EWMA per destination key (/24 + port) and per path, plus a global EWMA.
- Choice:
  - both paths measured: the faster one wins; the default path (relay) keeps ties within 10 %;
  - one or neither measured: the global comparison decides, else the default;
  - exploration: 5 % of connections on a decided key try the losing path, or 25 % when the loser's data is stale (> 6 h) or one path is unmeasured.
- Small connections never produce samples. They follow whatever the key or global evidence says.
- The metric is a screen, not a verdict: a slow sample may be a slow server or a slow reader. It catches a path that is clearly worse; it does not make fine distinctions.

## Fail-open

tandem sits under every TCP connection the laptop makes, so the rule is: when anything is wrong, traffic goes direct, quickly.

| Situation | Mechanism | Measured |
|---|---|---|
| Daemon crashes or is SIGKILLed | The nft table is created with the kernel's `owner` flag over the daemon's own netlink socket. The kernel deletes it when the process dies, however it dies. No cleanup code involved. | table gone in 7-29 ms; next request direct |
| Daemon hangs | `WatchdogSec=3`; the daemon pings systemd only while its accept loop answers a real connection. systemd kills it, the kernel drops the table. | table gone 1.95 s after SIGSTOP (10.08 s with the earlier 10 s setting). New connections made while it hangs wait that long. |
| archserver asleep (packets silently dropped) | Relay dial budget 300 ms, then direct. The first real connection that fails marks the relay down at once, so later connections skip the relay without waiting for the redirect to pause. | exactly one connection pays +300 ms (0.40 s vs 0.11 s baseline, container against the real relay); the next ones are at baseline |
| Relay process dead (RST) | Same, faster. | new connection direct in ~100 ms |
| Real relay service stopped (container client against archserver) | Refused dial, then direct. | no added latency: 0.10-0.11 s per request, same as baseline. The redirect itself pauses up to ~4 s later; nobody waits for that. |
| Another VPN owns the default route (OpenVPN, WireGuard, a Tailscale exit node, PPP, policy IPsec) | Every 2 s: `ip route get 1.1.1.1`; if it leaves through anything but a plain Ethernet/Wi-Fi device, or xfrm policies exist, the redirect is paused. | paused within 2 s |
| `tandem disable` | Flag file in `/var/lib/tandem`; survives reboots and restarts. | immediate |

One non-obvious detail, found by a failing test: **pausing never deletes the chain.** When the redirect stops, the table and its nat chain stay, plus a `ct state invalid return` rule that matches nothing in practice. Its real job is to hold a conntrack reference for the network namespace. Without it, removing the last `redirect` rule drops the last reference. The kernel then unregisters conntrack's hooks, and every connection redirected moments earlier stops being translated: its replies never reach the application (curl hung for 10 s in the test). On the laptop docker's iptables rules would hold a reference by luck; tandem holds its own. The table itself is deleted only when the daemon exits.

## Install

Through [ownbox](https://github.com/BogdanStamenovic/ownbox), on both machines:

```
ownbox install tandem
```

The setup is interactive on a terminal:

1. **Role.** Relay or client; the default is guessed from the hostname (`TANDEM_RELAY_HOST`, default `archserver`).
2. **Relay:** listen address (defaults to `0.0.0.0:7411`), allowed client prefixes (defaults to the LAN subnet; beacons go out on the addresses inside it), keep or regenerate the key.
3. **Client:** pair over ssh with the relay host. This runs `sudo -n tandem pair --export` there as the invoking user and fetches both the relay's address and the key. Alternatively paste the key (`sudo tandem pair --show` on the relay). Then an optional static relay address (a fallback for when beacons are not heard, used only on networks where the relay has answered), then an authenticated test ping; if it answers, this network is recorded as home.
4. **Start now and at boot?**

Without a terminal (`ownbox install tandem --yes`) nothing prompts. The answers come from environment variables or defaults, and every value taken is printed:

| Variable | Meaning |
|---|---|
| `TANDEM_ROLE` | `relay` or `client` |
| `TANDEM_RELAY_HOST` | host that is the relay by default, and the ssh pairing target |
| `TANDEM_RELAY` | client: relay `IP:port` |
| `TANDEM_LISTEN`, `TANDEM_ALLOW` | relay: listen `IP:port`, allowed prefixes (comma separated) |
| `TANDEM_PAIR_SSH` | client: ssh target to pair with |
| `TANDEM_PSK` | client: key in hex |
| `TANDEM_ENABLE` | `yes`/`no`; default for a client is yes only if the relay answered an authenticated ping with this key |
| `TANDEM_PURGE` | remove: `yes` deletes config, key and learned data (default keeps them) |

Lifecycle:

- `ownbox update tandem` rebuilds from vendored sources, replaces the binary and unit, and restarts the service if it was running. Config, key and learned data are kept.
- **Updating a pre-discovery install** (tested in the container lab, `test/migrate-ct.sh`, 22 checks): the relay's `listen` changes from its one IP to `0.0.0.0` on the same port, and the original config is kept as `tandem.json.pre-discovery`. The client config is untouched: its `relay` becomes the static fallback. If that address answers an authenticated ping during the update, the current network is recorded as home. `ownbox rollback` to a pre-discovery revision works on the migrated config (an older binary accepts `0.0.0.0:7411` and ignores `discovery.json`), and so does updating forward again.
- An older version started on newer learned data leaves the file untouched rather than downgrading it, so `ownbox rollback` is safe.
- `ownbox uninstall tandem` stops and removes the units, the binary and any rules, and asks before deleting `/etc/tandem` and `/var/lib/tandem`.

Root parts run through `sudo`. Without a terminal, sudo must be passwordless or the script stops with a message rather than hanging.

Files: `/usr/local/lib/tandem/tandem` (a root-owned copy: the root service never executes the user-writable ownbox checkout), `/etc/systemd/system/tandem-{client,relay}.service`, `/etc/tandem/{tandem.json,psk}`, `/var/lib/tandem/`.

**Removing everything by hand**, if tandem itself is broken:

```
sudo systemctl disable --now tandem-client   # the rules die with the process
sudo nft delete table ip tandem              # only if a table somehow remains
```

## Usage

```
tandem status               state, relay health, per-destination measurements, recent connections
tandem why HOST [--port N]  which path HOST's connections take and why (resolves all addresses)
tandem disable | enable     turn redirection off/on; --dry-run shows what would change
tandem force relay|direct|auto  pin new connections to one path for benchmarking (not persisted)
tandem rules                the nft rules the client installs
sudo tandem pair --ssh HOST re-pair a client; on the relay: --show, --export, --generate
```

Exit codes: 0 ok, 1 failure, 2 usage error, 3 daemon not running.

Any local user may run `disable`/`enable`: disabling only makes traffic go direct, and this is a single-user laptop.

## Mechanism demo (synthetic, not a headline number)

`sudo ./test/netns.sh` builds four network namespaces on one machine:

- a "laptop" behind a netem-shaped "Wi-Fi" link (3 ms delay and the given loss each way);
- a relay on a clean link;
- an "internet" server 40 ms RTT away.

It then runs the fail-open tests above (27 checks).

Download of a 64 MiB file, Mbit/s, two runs each. The first tandem run may be an exploration that went direct.

| Wi-Fi loss each way | Direct | Through tandem |
|---|---|---|
| 0.1 % | 428 / 463 | 523 / 523 |
| 0.5 % | 6 / 6 | 368 / 33 |
| 1 % | 4 / 4 | 385 / 417 |

The relay leg shares the lossy "Wi-Fi" link, so it can lose too: the 33 is a run where it did. The cliff between 0.1 % and 0.5 % direct is netem's random loss against CUBIC at a 46 ms RTT.

These only show the mechanism. The shaping is invented, and the real laptop numbers will be smaller in both columns.

## Small hardware

**Question:** could the relay run on a box the size of a Luckfox Pico Max (Rockchip RV1106G3: one Cortex-A7 at 1.2 GHz, 256 MB RAM, one 10/100 Ethernet port), and what does it cost to run? No board was available, so this is an emulation on archserver (Ryzen 5 5600). `sudo test/smallhw.sh` rebuilds all of it.

**Answer:** yes. On the Pico Max the 100 Mbit port is the bottleneck, not the CPU. The same CPU behind 1 GbE tops out at about 330 Mbit/s. Saturating 1 GbE takes about 3x that CPU.

### How it was emulated

- **A whole VM, not a capped process.** On archserver, 90 % of the relay's CPU cost is kernel receive softirq, and only 10 % is the relay process. At 855 Mbit/s the core was 33 % busy and the process 3.7 %. A cgroup CPU quota on the process does not throttle softirq, so it would have capped a tenth of the cost.
  - The relay therefore runs in a 1-vCPU, 256 MB KVM guest: the stock Arch kernel, a 4 MB Go `/init` (`test/vminit`), no busybox.
  - Only the vCPU thread is capped. QEMU's vhost and main-loop threads stand in for the NIC's DMA and run uncapped on another core.
  - The guest NIC has TSO/GSO off. The RV1106 MAC has no TSO, and veth/virtio would otherwise hand over 64 KiB packets and hide the per-packet cost.
- **CPU budget.** cpu.max gives the vCPU 1 ms per period. The period was calibrated with CoreMark on the same core under the same cap:

  | cpu.max | CoreMark/s | Stands for |
  |---|---|---|
  | none | 38,941 | one Zen 3 core at 4.44 GHz |
  | 1 ms / 10 ms | 3,203 | optimistic A7 (2.7/MHz) |
  | **1 ms / 13 ms** | **2,447** | **A7 at 1.2 GHz (2.04/MHz)** |
  | 1 ms / 20 ms | 1,582 | pessimistic, memory-bound A7 |

  - The A7 side is published, not measured here: CoreMark 1971 at 1 GHz on an Allwinner A20 and 1150 at 528 MHz on an i.MX6UL (pmeerw.net/armbench), i.e. 2.0-2.2/MHz.
  - Memory copy is a worse ratio: memcpy is 24.4 GB/s on this core (tinymembench) against 0.85-1.0 GB/s published for H3 and Pi 2 boards, about 27x.
  - The relay moves data with `splice`, and checksums are offloaded on the board, so per-packet integer work dominates. That is why CoreMark is the yardstick.
- **Link.** tbf shapes the relay's single link to 100 Mbit or 1 Gbit each way. Relayed bytes cross that one port twice, as on a one-port board.

### Results

Download through the relay, Mbit/s:

| Link | CPU budget | 1 connection | 8 connections | Bottleneck |
|---|---|---|---|---|
| 100 Mbit | A7 pessimistic / central / optimistic | 93 / 92 / 93 | 91 / 93 / 93 | link |
| 1 GbE | A7 pessimistic | 236 | 218 | CPU |
| 1 GbE | **A7 at 1.2 GHz** | **323-330** | **338** | **CPU** |
| 1 GbE | A7 optimistic | 424 | 445 | CPU |
| 1 GbE | 2x A7 | 550 | 668 | CPU |
| 1 GbE | 3x A7 | 631 | 927 | link |
| 1 GbE | 4x, 6x A7, uncapped | 640 | 930 | link |

- One connection levels off at ~640 Mbit/s even uncapped. That is TCP window against this lab's 10 ms RTT, not CPU.
- On the 100 Mbit port, the A7 runs at roughly 100/330, about 30 % busy.
- These numbers lean pessimistic: VM exits (each interrupt, each wake from idle) are charged to the capped vCPU, and a real board's interrupts are cheaper than VM exits.
- The published cross-check agrees: the NanoPi R1S-H3 (H3, A7 cores, USB gigabit) is quoted at 320-334 Mbit/s on its LAN port.

Latency through the relay, 10 ms WAN RTT, A7 budget:

| | Direct | Relay, uncapped | Relay, A7 budget |
|---|---|---|---|
| 1-byte round trip, idle, p50 | 10.04 ms | 10.19 ms | 10.16-10.19 ms |
| New connection plus one round trip, idle, p50 | 20.12 ms | 20.47 ms | 20.47 ms |
| Round trip under a saturating download, 100 Mbit, p50/p99 | n/a | 58 / 76 ms | 58 / 70 ms |
| Round trip under a saturating download, 1 GbE, p50/p99 | n/a | 50 / 51 ms | 50 / 79 ms |

- The slow CPU adds nothing at idle.
- Under load, the queue in the shaped link dominates: bufferbloat, the same with any CPU.
- The A7 budget shows only where it is saturated (1 GbE, p99 +28 ms).
- Part of that tail is the emulation's own fault: a 1 ms/13 ms cap stalls up to 12 ms at a time, and a slow CPU does not.

### What it costs

| Measure | Value | Notes |
|---|---|---|
| Relay process RSS | 5-10 MB idle, +~10 KB per connection, 25 MB peak at 1000 connections | Go runtime plus 3 goroutines per connection |
| Kernel memory | ~53 KB per open connection (drop in guest MemAvailable, 1 to 256 connections at 183 Mbit/s total) | socket buffers; grows with per-connection rate |
| Threads | 6-12, independent of connection count | Go runtime |
| File descriptors | ~3.4 per connection steady, up to 6 while both directions splice | 2 sockets plus Go's splice pipes |
| Guest RAM | 201 MB MemTotal, ~145 MB available idle, ~105 MB just after 1000 connections | 256 MB guest with an 11 MB initramfs held in RAM |
| CPU, link-paced, process only | ~6.5 ns/B at 200 Mbit/s, ~2 ns/B at 1 Gbit/s (Zen 3) | per-wakeup cost; a paced link delivers one MSS per wakeup |
| CPU, whole guest, unpaced | 1.1-1.4 ns/B, about 5-6 cycles per relayed byte (Zen 3) | batched; the floor, not the typical case |
| Binary, ARMv7 (`GOARM=7`, static) | 6.5 MB, 4.4 MB stripped | the install already strips (`-s -w`) |

- **Cost is per packet, not per byte.** The same relay costs ~6.5 ns/B paced at 200 Mbit/s and ~0.4 ns/B unpaced, because a paced link wakes it once per packet.
- **Cycles per byte has no single honest number.** Native on archserver, the core's figure includes the emulated router's forwarding and the shaper's timers (veth runs the receiver's work on the sender's CPU), and the process figure leaves out the kernel. The VM figures above are the isolated ones.

### What changed because of this

- **Kept: `splice` (`io.Copy` between TCP sockets).** It was measured head-to-head against read/write with 16, 32 and 128 KiB buffers: same throughput, process CPU within run-to-run noise (±10 %). Splice also avoids a memcpy, and memcpy is relatively the A7's weakest point (~27x slower than Zen 3, against ~16x for integer work). After a burst, Go's pipe pool keeps the idle splice pipes until garbage collection empties it (measured: 812 pipe descriptors still open 9 s after 300 connections ended, with every socket closed). New connections reuse them first, so the count is bounded by peak concurrency. It is retention, not a leak.
- **Fixed: running out of file descriptors.**
  - Under a busybox init the hard RLIMIT_NOFILE is the kernel default 4096. At 6 descriptors per connection that runs out near 680 connections, while the relay allowed 2048.
  - In the VM, 1000 connections produced `accept4: too many open files` and EMFILE upstream dials, and throughput fell to 58 Mbit/s.
  - Now the relay raises its own limit to what 2048 connections need (12352). Where it cannot, it sizes its connection cap to the descriptors it has. Example: a non-root relay at 1024 files caps at 160 connections, and the other 240 of 400 got "relay overloaded", which the client sends direct, with 0 EMFILE.
  - After the fix, 1000 connections through the A7-budget VM: 0 errors.

### Hardware that would not be the bottleneck at 1 GbE

**One core of about 3x an A7 at 1.2 GHz** (CoreMark ~7,300) saturates 1 GbE with several connections. Candidates, not tested:

| Board | SoC | Price (published) | Why it should do | Confidence |
|---|---|---|---|---|
| Orange Pi Zero 3 | H618, 4x A53 1.5 GHz, GbE | $15-25 | PassMark single-thread 396 vs 179 for an A7 at 1.2 GHz: ~2.2x per core, 4 cores | estimate; one core alone falls short of 3x, so it needs the kernel stack spread across cores (several flows, RPS) |
| NanoPi R2S | RK3328, 4x A53 1.3 GHz, GbE | ~$30 | 940 Mbit/s NAT routing reported (OpenWrt forum) | NAT forwarding is cheaper than terminating TCP twice |
| NanoPi R3S | RK3566, 4x A55, 2x GbE | $30-35 | 934 Mbit/s vendor iperf | vendor number |

- Any single-core A7 board (Pico Max, Pico Mini, RV1103/1106) is CPU-bound near 330 Mbit/s at 1 GbE, which is moot on their 100 Mbit ports.
- Archserver as the relay is far from its limit: 1.9 Gbit/s unpaced through the native relay (16 connections, offloads off) used 73 % of its core.

### Running it on a Luckfox (not built)

The stock image is Buildroot with busybox init and no systemd. What a port would need:

| Need | Note |
|---|---|
| Binary | `GOARCH=arm GOARM=7 CGO_ENABLED=0`; static, 4.4 MB |
| Init script | `/etc/init.d/S50tandem` starting `tandem run` with `TANDEM_CONFIG`, restart on exit (a `while` loop or busybox `start-stop-daemon`). No sd_notify or watchdog: both are already no-ops without `NOTIFY_SOCKET`. |
| File limit | Handled by the fix above (the relay raises it as root); `ulimit -n 12352` in the script is belt and braces. |
| ownbox | ownbox's lifecycle assumes git, a shell and systemd on the target. Realistic shape: archserver cross-builds and pushes the binary and init script over ssh, so the manifest's `install.setup` and `install.update` run on the build host, not on the board. Not designed yet. |
| RAM reservation | The stock RV1106 image reserves memory for the camera pipeline (~66 MB by the Luckfox FAQ for other Pico models), which is free to reclaim for a relay. |

## Why Go

- One static binary for both roles. Nothing on the laptop needs a runtime.
- `SO_ORIGINAL_DST`, `SO_MARK`, `TCP_USER_TIMEOUT` and netlink are all reachable without cgo.
- Goroutine-per-connection is the natural shape for a proxy. `io.Copy` between two TCP sockets uses `splice` on the relay.
- It was already installed on both machines.

Rust was the alternative and is not installed here.

The one dependency patch: `google/nftables` v0.3.0 sends table flags as a hardcoded zero, so it cannot create an owned table. `third_party/nftables` is upstream with that one line changed. Everything is vendored, so builds need no network.

## Limitations

**Not built in v1**

- **IPv4 TCP only.** IPv6 is never redirected (the laptop has no global IPv6 today). UDP is never touched, so QUIC/HTTP3 (much of Google, Cloudflare and YouTube in browsers) stays direct. A later option is to block outbound UDP 443 so browsers fall back to TCP; it is not built.
- **Only the laptop's own traffic.** Docker containers and VMs on the laptop are forwarded traffic and are not redirected.
- **No encryption on the LAN leg.** Plain HTTP (some pacman mirrors) crosses the home LAN in clear between the laptop and archserver. It would cross the internet in clear anyway.
- **Only one relay.** No pool: two relays with the same key on one LAN would both be heard and the client would follow whichever spoke last.
- **Home network only.** Away from home there is no relay and tandem stays paused, sending nothing. It does not relay over the tailnet.
- **The relay must be on the client's subnet.** Broadcast does not cross a router, and the fallbacks are only used for an address inside the client's own subnet (an off-subnet address would send the probe to the gateway, i.e. out of the network). A routed relay needs `"discovery": false`.

**Behaviour to know about**

- **In-flight relayed connections drop when archserver dies or sleeps mid-transfer.** With split TCP the connection's state lives on the relay. Keepalive and `TCP_USER_TIMEOUT` (20 s) make them fail instead of hanging for 15 minutes. Applications that retry (pacman, curl `--retry`, browsers) recover; new connections go direct immediately.
- **The application sees "connected" before the real connection exists.** A destination that refuses the connection shows up as a reset after connect, not as "connection refused".
- **A hung daemon stalls new connections for up to the watchdog interval** (3 s) before systemd kills it.
- **Restarting the client cuts proxied connections** after an 8 s drain. This includes `ownbox update`.
- **Measurements are coarse.** The key is /24 + port, so a CDN's different customers in one /24 share a verdict. Throughput samples include slow servers and slow readers. Hosts that are only ever touched with short connections are never measured and follow the global evidence.
- **VPN detection is heuristic.** It looks at the default route's device type and xfrm policies. A split-tunnel VPN that leaves the default route alone does not pause tandem, but its traffic leaves through its own interface, and the redirect only matches the physical uplink, so that traffic is untouched.
- **Clock skew over 120 s** between the machines makes the relay reject the client and the client reject the beacons. Both run NTP.
- **A replaced router is a new network.** The fingerprint is the gateway's MAC, so after a router swap the cached and static fallbacks are not used until a beacon or answered query re-verifies the relay there. Beacons themselves keep working. Several access points behind one router share a gateway and so a fingerprint.
- **Broadcast filtered on a network never verified as home** (a fresh install behind client isolation, with no static address answering at setup): the client stays paused. `sudo tandem pair --key KEY` with a static address records the network once the relay answers.
- **Beacon traffic:** one 91-byte broadcast per relay address every 5 s; a query is 60 bytes, three at most per event.
- **Relay overload:** at most 2048 concurrent upstream connections, fewer if the relay cannot get 6 file descriptors per connection (it logs the number at start). Beyond the cap the client goes direct.
- **Small-hardware numbers are emulated.** No Luckfox or other ARM board was tested; see [Small hardware](#small-hardware) for how and how far to trust it.

## Status

- Relay role: installed through ownbox on archserver and running.
- Client role: installed on the laptop on 2026-11-18 with a fresh interactive `ownbox install tandem` (ssh pairing with archserver worked), running. Numbers are in [Measured on the real laptop](#measured-on-the-real-laptop).
- Also exercised in network namespaces (27/27) and in a booted systemd-nspawn container on archserver, against the real relay, under the real unit file.

## License

MIT
