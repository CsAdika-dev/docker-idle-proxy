=== DOCKER IDLE PROXY (DIP) ===


A tiny on-demand TCP proxy for Docker services.
Starts a Docker Compose service when a client connects, proxies the
connection, and shuts the service down after it has been idle.
Service-specific logic is intentionally externalized through startCMD,
watchCMD and shutdownCMD. The proxy doesn't need to know what runs behind it
— you decide how to integrate it.
No web UI. No database. No Kubernetes. No unnecessary machinery. Just a small
daemon doing one job.

Originally built because I got tired of manually starting and stopping my
home Minecraft servers.
It turned out to be useful as a generic Docker-backed lazy TCP proxy too.


1. OVERVIEW
------------------------------------------------------------------------
Docker Idle Proxy is an automated TCP proxy daemon written in Go.
It monitors directories containing Docker Compose stacks, dynamically listens
on their configured ports, and automatically starts or stops containers based
on incoming network traffic and internal user activity (e.g., RCON queries).

Key Features:
- On-Demand Startup: Intercepts incoming TCP connections and starts the stack.
- Auto-Shutdown: Monitors idle duration and custom check commands (e.g., RCON
user count).
- Live File Watching: Tracks directory creations, modifications, and deletions.
- Hot Reloading: Reloads global configurations and stack overrides dynamically.
- Prometheus Metrics: Exposes real-time runtime metrics.
- Sensitive Log Sanitization: Automatically masks passwords, tokens, and
secrets in logs.


2. CONFIGURATION (config.yaml)
------------------------------------------------------------------------
Global settings are read from `config.yaml` (default paths:
`/etc/docker-idle-proxy/` or current working directory).

Example global configuration:

workdir: "/srv/minecraft-servers"
idletime: 600
cooldown: 120
check_interval: 15
metrics_port: ":9090"
ref_val: "0"
start_cmd: "docker compose -f {{.Path}}/docker-compose.yml up -d"
watch_cmd: "mcli -H 127.0.0.1 -P {{.TargetPort}} -p '{{.Env.RCON_PASSWORD}}' playerlist | grep -oP '\\d+' | head -n1"
shutdown_cmd: "docker compose -f {{.Path}}/docker-compose.yml down"


3. DOCKER COMPOSE OVERRIDES (x-dip-*)
------------------------------------------------------------------------6. DONATE / SUPPORT
------------------------------------------------------------------------
If docker-idle-proxy saved you a few watts, a few euros, or a few hours of
your time, feel free to buy me a cold beer, coffe or imperial star destroyer:

  - Solana (SOL)        : MiRMtwPWxt2v5YH1hWgq24iJyLNjsfJQQMtjT4MRuYy
  - TRC-20 (USDT/TRX)   : TUs4o3AjBZUvhmxWu3TymyiEXbv2bhKwLr
  - BEP-20 (BNB/Tokens) : 0x77CE04b2F3C2220A559657D673d706FF60F6E5D8

Cheers!

The proxy evaluates ONLY the VERY FIRST service defined under `services:` in
`docker-compose.yml`. If the first service lacks a `ports` section, the compose
file is skipped.
(docker-compose path: <config.yaml:workdir>/<folder>/docker-compose.y(a)ml)
(.env path: <config.yaml:workdir>/<folder>/.env)

Per-stack settings can be overridden directly in `docker-compose.yml` using
custom `x-dip-*` fields:

services:
  mc-paper:
    container_name: paper-server-01
    ports:
      - "25565:25565"
    x-dip-listen_port: "25565"
    x-dip-idletime: 900
    x-dip-cooldown: 300
    x-dip-check_interval: 10
    x-dip-ref_val: "0"
    x-dip-watch_cmd: "mcli -H 127.0.0.1 -P {{.TargetPort}} -p '{{.Env.RCON_PASSWORD}}' playerlist"


4. EXECUTABLE USAGE
------------------------------------------------------------------------
Run the binary using:

  ./docker-idle-proxy -c /path/to/config.yaml

If `-c` is omitted, it will automatically search in:
  1. /etc/docker-idle-proxy/config.yaml
  2. /etc/docker-idle-proxy/config.yml
  3. ./config.yaml


5. DESIGN PHILOSOPHY
------------------------------------------------------------------------
Idle Proxy intentionally keeps service-specific integration outside of the
proxy itself. The proxy provides the lifecycle and networking primitives
required to run an on-demand TCP service:

  - startCMD    : Start the service stack
  - watchCMD    : Query whether the service is actively being used
  - shutdownCMD : Gracefully stop the service stack
  - refVal      : Define what the watchCMD result means for the idle state

These commands are deliberately user-defined. Idle Proxy does not need to know
whether the target service is Minecraft, Valheim, a development environment, or
a custom internal tool.

Core Principle:
"The proxy handles the lifecycle; the user handles the integration."

Instead of baking service-specific adapters into the core codebase, users can
wire up existing scripts, CLI tools, APIs, or native shell commands via the
command hooks. This keeps the core lightweight, generic, and decoupled from
whatever service runs behind it.


6. DONATE / SUPPORT
------------------------------------------------------------------------
If docker-idle-proxy saved you a few watts, a few euros, or a few hours of
your time, feel free to buy me a cold beer, coffee or an Imperial Star 
Destroyer :D

  - Solana (SOL)        : MiRMtwPWxt2v5YH1hWgq24iJyLNjsfJQQMtjT4MRuYy
  - TRC-20 (USDT/TRX)   : TUs4o3AjBZUvhmxWu3TymyiEXbv2bhKwLr
  - BEP-20 (BNB/Tokens) : 0x77CE04b2F3C2220A559657D673d706FF60F6E5D8

Cheers!
