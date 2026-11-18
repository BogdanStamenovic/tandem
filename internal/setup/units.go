package setup

const clientUnit = `# Installed by tandem setup. Rewritten on every upgrade; edit with a drop-in.
[Unit]
Description=tandem client: relay this machine's TCP through a LAN relay when faster
Documentation=https://github.com/BogdanStamenovic/tandem
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
Environment=TANDEM_CONFIG=/etc/tandem/tandem.json
ExecStart=/usr/local/lib/tandem/tandem run
# The redirect table is owned by the daemon's netlink socket, so the kernel
# removes it when the process dies however it dies. This line only catches a
# table left by something else (a manual test, an old version).
ExecStopPost=-/usr/bin/nft delete table ip tandem
Restart=always
RestartSec=2
# The daemon pings the watchdog only while its accept loop answers a real
# connection; a hung daemon is killed, and its rules die with it.
WatchdogSec=10
TimeoutStopSec=15
StateDirectory=tandem
RuntimeDirectory=tandem
CapabilityBoundingSet=CAP_NET_ADMIN
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
MemoryMax=256M

[Install]
WantedBy=multi-user.target
`

const relayUnit = `# Installed by tandem setup. Rewritten on every upgrade; edit with a drop-in.
[Unit]
Description=tandem relay: dial out for paired LAN clients
Documentation=https://github.com/BogdanStamenovic/tandem
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
DynamicUser=yes
LoadCredential=psk:/etc/tandem/psk
Environment=TANDEM_CONFIG=/etc/tandem/tandem.json
ExecStart=/usr/local/lib/tandem/tandem run --psk-file ${CREDENTIALS_DIRECTORY}/psk
Restart=always
RestartSec=2
WatchdogSec=30
RuntimeDirectory=tandem
CapabilityBoundingSet=
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LimitNOFILE=16384
MemoryMax=512M

[Install]
WantedBy=multi-user.target
`
