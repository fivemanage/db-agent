package service

// Keep the updater outside the main service's cgroup via systemd-run. A private
// /tmp or read-only /etc would break supported explicit config and state paths.
const systemdScript = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
User=root
UMask=0077
NoNewPrivileges=yes
KillMode=control-group
TimeoutStopSec=20
WorkingDirectory=/
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=db-agent
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment=DB_AGENT_SERVICE_NAME=db-agent

[Install]
WantedBy=multi-user.target
`
