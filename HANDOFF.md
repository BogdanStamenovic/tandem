# tandem: handoff (2026-11-18, tandem-3f)

What tandem is, how it works and its limitations are in README.md. This file covers the state of things, the laptop install procedure and how to recover.

## State

| Piece | State |
|---|---|
| Repo | https://github.com/BogdanStamenovic/tandem (public), `main` |
| archserver relay | Installed via `ownbox install tandem` (interactive), updated via `ownbox update` to the latest commit. `tandem-relay.service` is enabled at boot. It listens on `192.168.1.139:7411` only and allows `192.168.1.0/24`. The key was rotated after the lab tests (the lab key passed through a tmux pane). |
| Laptop (`ssh arch`, hostname `bogdan`) | **Nothing installed. Nothing changed.** Only read-only probes were run before it went down. |
| Lab | Torn down. `ip_forward` back to 0, no lab nft table, no container, no mounts. `test/container-lab.sh up/down` recreates it. |

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
| Real relay stopped | Requests fine; paused in 3.9 s; restored 2 s after it came back. |

**archserver's own direct numbers** (the ceiling the relay can offer the laptop, `test/bench.sh`), Mbit/s:

| Target | Mbit/s |
|---|---|
| Hetzner fsn1 | 187-212 |
| OVH | 361-374 |
| Hetzner Ashburn | 54-57 |
| Fastly mirror, `extra.files.tar.gz` | 274-279 |
| `pacman -Sw python go` | 217-243 |

## Not done

- **Real laptop numbers** (README headline): direct vs relayed with `test/bench.sh`. This is the first thing to do.
- **Laptop to archserver ssh pairing.** Never tested; the laptop went down before it could be probed. The paste path works.

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
