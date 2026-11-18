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

Numbers from tandem itself on the real laptop: **not measured yet** (see [Status](#status)).

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

- It listens on the LAN address only and accepts a source only if it is in the allow list *and* proves the pre-shared key (HMAC-SHA256 over a timestamp, a nonce and the destination, with replay protection).
- The reply is authenticated too, so a LAN impostor cannot pose as the relay.
- It refuses private, loopback and tailnet destinations, so it cannot be used to reach archserver's own services or its LAN.
- It is not an open proxy.
- It does not encrypt: the payload is whatever the application sent, usually TLS already.

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
| Daemon hangs | `WatchdogSec=3`; the daemon pings systemd only while its accept loop answers a real connection. systemd kills it, the kernel drops the table. | earlier build with 10 s watchdog: killed at 10.08 s. New connections made while it hangs wait up to the watchdog interval. |
| archserver asleep (packets silently dropped) | Relay dial budget 300 ms, then direct; health marks the relay down and the redirect is paused. | first new connection direct in 400 ms; redirect paused 424 ms later |
| Relay process dead (RST) | Same, faster. | new connection direct in ~100 ms |
| Real relay service stopped (container client against archserver) | | first request 0.11 s, redirect paused after 3.9 s, restored 2 s after the relay returned |
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
2. **Relay:** listen address (defaults to the LAN address), allowed client prefixes (defaults to the LAN subnet), keep or regenerate the key.
3. **Client:** pair over ssh with the relay host. This runs `sudo -n tandem pair --export` there as the invoking user and fetches both the relay's address and the key. Alternatively paste the key (`sudo tandem pair --show` on the relay). Then the relay address, then an authenticated test ping.
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

It then runs the fail-open tests above (25 checks).

Download of a 64 MiB file, Mbit/s, two runs each. The first tandem run may be an exploration that went direct.

| Wi-Fi loss each way | Direct | Through tandem |
|---|---|---|
| 0.1 % | 428 / 463 | 523 / 523 |
| 0.5 % | 6 / 6 | 368 / 33 |
| 1 % | 4 / 4 | 385 / 417 |

The relay leg shares the lossy "Wi-Fi" link, so it can lose too: the 33 is a run where it did. The cliff between 0.1 % and 0.5 % direct is netem's random loss against CUBIC at a 46 ms RTT.

These only show the mechanism. The shaping is invented, and the real laptop numbers will be smaller in both columns.

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
- **Only one relay.** No discovery and no pool.
- **Home network only.** Away from home the relay is unreachable and tandem stays paused. It does not relay over the tailnet.

**Behaviour to know about**

- **In-flight relayed connections drop when archserver dies or sleeps mid-transfer.** With split TCP the connection's state lives on the relay. Keepalive and `TCP_USER_TIMEOUT` (20 s) make them fail instead of hanging for 15 minutes. Applications that retry (pacman, curl `--retry`, browsers) recover; new connections go direct immediately.
- **The application sees "connected" before the real connection exists.** A destination that refuses the connection shows up as a reset after connect, not as "connection refused".
- **A hung daemon stalls new connections for up to the watchdog interval** (3 s) before systemd kills it.
- **Restarting the client cuts proxied connections** after an 8 s drain. This includes `ownbox update`.
- **Measurements are coarse.** The key is /24 + port, so a CDN's different customers in one /24 share a verdict. Throughput samples include slow servers and slow readers. Hosts that are only ever touched with short connections are never measured and follow the global evidence.
- **VPN detection is heuristic.** It looks at the default route's device type and xfrm policies. A split-tunnel VPN that leaves the default route alone does not pause tandem, but its traffic leaves through its own interface, and the redirect only matches the physical uplink, so that traffic is untouched.
- **Clock skew over 120 s** between the machines makes the relay reject the client. Both run NTP.
- **Relay overload:** at most 2048 concurrent upstream connections; beyond that the client goes direct.

## Status

- Relay role: installed through ownbox on archserver and running.
- Client role: exercised in network namespaces (25/25) and in a booted systemd-nspawn container on archserver, against the real relay, under the real unit file.
- Not yet installed on the laptop; real before/after numbers there (including a `yay -Syu`-sized download) are the next step.

## License

MIT
