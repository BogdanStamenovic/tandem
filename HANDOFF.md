# tandem: handoff (2026-11-18, tandem-3f)

What tandem is, how it works and its limitations are in README.md. This file covers the state of things, the laptop install procedure and how to recover.

## State

| Piece | State |
|---|---|
| Repo | https://github.com/BogdanStamenovic/tandem (public), `main` |
| archserver relay | `ownbox update` to f03e162 (2026-11-18 ~17:58 UTC; later commits are test tools and docs). The migration moved `listen` from `192.168.1.139:7411` to `0.0.0.0:7411`, and the original is kept as `/etc/tandem/tandem.json.pre-discovery`. Key hash unchanged (788bceb7...). A second update left the config byte-identical. It beacons from 192.168.1.139/24 every 5 s. Captured on enp4s0 and verified with the real key (`discotool verify`), and it answered a signed query (`discotool query`). Allows `192.168.1.0/24`. `tandem-relay.service` is enabled at boot. |
| Laptop (`ssh arch`, hostname `bogdan`) | **Still on d92c6d1 (pre-discovery); the discovery update is pending.** At 15:5x UTC it was on a foreign network (192.168.4.108/22, gateway 192.168.4.1), and the old client was probing 192.168.1.139 through that gateway. It went offline around 17:20 UTC before the update. An attempt to add a count-only nft table `ip tandem_audit` (output hook, policy accept, counters only) timed out as the link died. It may or may not exist: check with `sudo nft list tables` and delete it with `sudo nft delete table ip tandem_audit`. A wake task `tandem-laptop-back` (every 15 min, `~/.local/state/tandem-laptop-watch.sh`) messages tandem-3f once ssh works again. Cancel it with `wake cancel tandem-laptop-back` after the update. Install details: installed 2026-11-18 ~10:28 UTC with an interactive `ownbox install tandem`; the nft ruleset before install is at `~/tandem-laptop-ruleset-before.nft` on archserver. ownbox is in `~/.local/bin`, which is not on a non-interactive ssh PATH. |
| Lab | Torn down. `ip_forward` back to 0, no lab nft table, no container, no mounts. `test/container-lab.sh up/down` recreates it. The small-hardware lab (`test/smallhw.sh`) is torn down too: no netns, no VM unit, no cgroups, `kvm.halt_poll_ns` back to 200000, cpu5 frequency limits back to 1750500-4470508 kHz. |
| Relay fd-limit fix | In the repo (README "Small hardware"), not deployed to archserver: under systemd the unit's open-file limit is already ample, so it changes only the startup log line there. The next `ownbox update tandem` picks it up. |

## Tested, with numbers

**Netns integration test** (`sudo test/netns.sh`): 27 checks, all passing, run repeatedly at 0.1 %, 0.5 % and 1 % emulated Wi-Fi loss.

| Check | Result |
|---|---|
| kill -9 of the daemon | The kernel drops the owned table in 7-29 ms; traffic goes direct. |
| Relay asleep (silent drop) | First new connection goes direct in 400 ms; redirect paused 424 ms later; restored about 2 s after it wakes. |
| Relay process dead (RST) | New connection goes direct in about 100 ms. |
| Another VPN on the default route | Paused within 2 s, restored when it goes away. |
| `disable` | Persists across restarts. |
| `--dry-run` | Changes nothing. |
| In-flight 8 MiB transfer across disable/enable | Byte-identical. |
| Relay hardening | Rejects bad keys and foreign sources. |
| Learned stats | Saved on stop. |
| SIGTERM | Leaves no table. |

**Synthetic speed demo** (mechanism only, invented shaping), Mbit/s:

| Loss each way | Direct | Through tandem |
|---|---|---|
| 1 % | 4 | 385-417 |
| 0.5 % | 6 | 33-368 |
| 0.1 % | 428-463 | 523 |

**Container stand-in.** A booted systemd-nspawn container used the real relay, the real unit and the ownbox lifecycle.

Installs:
- Fresh interactive install: role defaulted to client; ssh pairing failed cleanly, then the pasted key worked; ping 300 µs; enabled.
- Fresh `--yes` install with `TANDEM_PSK`/`TANDEM_RELAY` set: enabled because the ping succeeded.
- `--yes` install with no key: no prompt and no hang; installed but not enabled.

Lifecycle:
- `ownbox update` on a populated install: key and config hashes identical, learned stats kept, new unit applied.
- `ownbox rollback`: older revision's update ran; stats kept; service back.
- Interactive uninstall: asked about purge, default keep, no rules left.
- `--yes` uninstall with `TANDEM_PURGE=yes`: everything gone.

Fail-open under systemd:

| Test | Result |
|---|---|
| SIGKILL | Table gone in 29 ms, auto-restart, redirect back in 3.3 s. |
| SIGSTOP | Watchdog killed it at 10 s, so WatchdogSec was cut to 3 s. Re-measured with 3 s (systemd-run in a netns): table gone after 1.95 s, Result=watchdog. |
| Real relay stopped | No added latency (0.10-0.11 s per request = baseline). The redirect pauses up to ~4 s later, which nothing waits for. |
| Real relay asleep (silent drop) | After the change in baf45b1: exactly one request pays +300 ms (0.40 s), the rest at baseline. Before it, 2 of 4 did. |

**archserver's own direct numbers** (the ceiling the relay can offer the laptop, `test/bench.sh`), Mbit/s:

| Target | Mbit/s |
|---|---|
| Hetzner fsn1 | 187-212 |
| OVH | 361-374 |
| Hetzner Ashburn | 54-57 |
| Fastly mirror, `extra.files.tar.gz` | 274-279 |
| `pacman -Sw python go` | 217-243 |

## On the laptop (2026-11-18)

| Check | Result |
|---|---|
| Interactive install | Role defaulted to client; ssh pairing with archserver worked (over the tailnet, got the LAN address back); authenticated ping 9.5 ms; enabled. |
| `tandem why` | Hetzner relayed; tailnet 100.72.2.62, LAN 192.168.1.139 and Moonlight port 47989 direct. `tailscale status` unchanged (archserver "active; direct"). |
| SIGKILL during a request loop | Table gone within 200 ms; 16/16 requests 200 with no gap; restarted with the table back in under 4 s. |
| Relay stopped on archserver for 6 s | 20/20 requests 200 at baseline latency (0.27-0.42 s); 1 fell back to direct; relay back up 5 s after restart. |
| `ownbox update tandem --yes` (10:39 UTC, d1f0535 to d92c6d1) | Key and config hashes identical, learned stats kept (relay 143 Mbit/s n=28, direct 101 n=27 before and after), service restarted on the new binary, still enabled, laptop online. |
| `test/bench.sh 3` | README, "Measured on the real laptop". Hetzner fsn1 +63 %, OVH -23 %, mirror +20 %, pacman +12 % (medians). |

## Discovery (2026-11-18 evening)

Asked: "yeah lets do something along the lines of discovery" (the relay IP is DHCP; a change silently disabled tandem, and on foreign networks the client probed .139). README "How it works / Discovery" and "Limitations" cover the design. Tests:

| Where | Result |
|---|---|
| `sudo test/netns.sh` | 47/47 on the last two runs |
| `test/migrate-ct.sh a0062b9 HEAD` (after `test/container-lab.sh up`) | 22/22 |
| archserver, live | beacons verified on the wire; query answered |

The laptop update has not been done; see State.

Notes for whoever continues:
- **The suite's flakiness is old.** The pre-discovery revision fails the post-speed-test checks in about 2 of 3 runs, because the saturated 1 %-loss link makes the fail-open probes pause the redirect for ~2 s. Measured 2-3 such flaps per run. The suite now waits for the rules before those checks.
- **bash reads a running script as it goes.** Editing `netns.sh` while it runs mixes old and new steps. Run a copy (`BIN=... bash copy.sh`) if you need to keep editing.
- **`machinectl copy-to` will not overwrite.** `migrate-ct.sh` deletes the old bundle first; without that it silently tested a stale commit.
- **tcpdump is not installed.** Captures use nft counters (netns) or an AF_PACKET socket bound to ETH_P_ALL. ETH_P_IP does not see outgoing frames.
- **Laptop update plan:**
  1. Count-only audit table for `ip daddr 192.168.1.139` and TCP/UDP 7411.
  2. 60 s of counts on the old client (expect a leak if it is away from home).
  3. `PATH=$HOME/.local/bin:$PATH ownbox update tandem --yes`.
  4. Reset the counters; 60 s should show 0 away from home. At home, `tandem status` should show the relay from beacon or query, and a relayed download should work.
  5. Delete the audit table.

## Small hardware (2026-11-18 afternoon)

Asked: could the relay run with the compute of a Luckfox Pico Max (he first said Mini, corrected to Max)? The full write-up is in README "Small hardware". In short:

- An A7-class single core is fine behind its own 100 Mbit port, and tops out near 330 Mbit/s behind 1 GbE.
- 1 GbE needs about 3x that CPU.
- The data path already uses splice; read/write was no better.
- One real fix: the relay now fits its connection cap to RLIMIT_NOFILE (EMFILE at 1000 connections under a bare init).

Method notes for whoever reruns it:
- A cgroup quota on the relay process is not an emulation: 90 % of the cost is kernel softirq it does not throttle. Use the VM mode.
- amd-pstate-epp in active mode ignores per-core `scaling_max_freq`, so downclocking a core does not work here.
- cpu.max takes at least 1 ms of quota, hence "1 ms per N ms" with N calibrated by CoreMark (13000 = A7 at 1.2 GHz).
- CoreMark's iteration count is its 4th argument. Under a cap, auto-sizing (0) gives nonsense.
- After switching the relay address between the VM and the native relay, flush the router namespace's neighbours, or the old MAC blackholes it.

## Not done

- **Longer real-world numbers.** Three rounds on Wi-Fi only show differences over ~20 %. A run at a worse time of day (the prototype's 39 Mbit/s case) would say more.
- **A real ARM board.** All small-hardware numbers are emulated; a Pico Max (or an H618 board for the 1 GbE case) on the bench would replace them. Packaging for a busybox target (init script, cross-build-and-push in place of ownbox on the board) is described in README, not built.
- **The laptop's discovery update** (see State) and a real-pair proof at home: status shows beacon/query, relayed download works.
- **`ownbox uninstall` on the laptop.** Exercised in the container only; he keeps tandem installed, so it was deliberately not run there.

Bugs found and fixed on the way, so nobody "simplifies" them back:

1. Pausing must not delete the nat chain, and a `ct` expression must stay in the chain. Dropping the last conntrack reference unregisters conntrack for the netns and breaks every connection redirected before the pause.
2. Rules are swapped by flushing the chain, never by deleting and re-adding the table, which briefly unregisters the NAT hook.
3. Fresh setup had to fill config defaults.
4. The lifecycle script has to build without `HOME`.

## Laptop install

Needs his go-ahead (verified Discord message). Before the first install, tell manager-17 and post in the channel, including the recovery section below.

Read-only preflight, from archserver:

```
ssh arch 'hostname; ip -br a; ip route; sudo nft list tables; systemctl is-active tailscaled; \
          ssh -o BatchMode=yes -o ConnectTimeout=5 archserver true && echo SSH-PAIRING-POSSIBLE; \
          getent ahostsv4 archserver'
ssh arch 'sudo nft list ruleset' > ~/tandem-laptop-ruleset-before.nft   # for comparison afterwards
```

Install: interactive, driven through tmux from archserver.

```
tmux new-session -d -s tl -x 200 -y 50 "ssh -t arch 'ownbox sync && ownbox install tandem'"
# Continue? y
# role: Enter (defaults to client: hostname bogdan is not archserver)
# pair over ssh with [archserver]: Enter if SSH-PAIRING-POSSIBLE above, else type -
# if pasting: put the key in a tmux buffer without echoing it in your own output:
umask 077; sudo cat /etc/tandem/psk | tr -d '\n' > /tmp/k && tmux load-buffer -b k /tmp/k && rm /tmp/k && tmux paste-buffer -d -b k -t tl && tmux send-keys -t tl Enter
# relay address: 192.168.1.139:7411 (setup only accepts and defaults to private IPv4, so a tailnet answer from MagicDNS is skipped; in the container lab it defaulted to the veth address, so read it before accepting)
# expect "answered an authenticated ping", then Start now? y
```

What it creates on the laptop (no existing `/etc` file is modified):
- `/etc/tandem/{tandem.json,psk}`
- `/etc/systemd/system/tandem-client.service`
- `/usr/local/lib/tandem/tandem`
- `/var/lib/tandem/`
- at runtime, `table ip tandem`

Verify:

```
ssh arch 'tandem status; tandem why fsn1-speed.hetzner.com; sudo nft list table ip tandem'
ssh arch 'tailscale status | head -3; tandem why 100.72.2.62'          # tailnet must say DIRECT
ssh arch 'sudo systemctl kill -s KILL tandem-client; sleep 0.2; sudo nft list tables'  # no "tandem" table
sudo systemctl stop tandem-relay   # on archserver: laptop must stay online; then:
sudo systemctl start tandem-relay
```

Measure (README "Status" and the prototype table get replaced by these):

```
ssh -t arch 'cd ~/.local/share/ownbox/tools/tandem && ./test/bench.sh 3'
```

## Recovery (laptop)

Stop the service; the kernel removes the rules with the process:

```
sudo systemctl disable --now tandem-client
sudo nft delete table ip tandem        # only if a table is somehow still listed by: sudo nft list tables
```

Full removal: `ownbox uninstall tandem` (it asks whether to delete `/etc/tandem` and `/var/lib/tandem`). Softer options: `tandem disable` turns redirection off without stopping anything, and `tandem force direct` sends everything direct until the next restart.

Note for reading laptop logs: the laptop clock is CET (UTC+1); archserver's shell times are UTC.
