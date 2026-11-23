<div align="center">
  <img src="https://avatars.githubusercontent.com/u/245985800?s=200&v=4" alt="FasterEdge logo" width="100" />
  <h2>FasterEdgeRunningCore</h2>
  <h3>FERC - a single-command deployment core for edge nodes</h3>
</div>

### 1. What it is

`FERC` (`ferc`) is the **running core** of the FasterEdge ecosystem: install it once on any operating system, let it provision the node's dependencies, and then it **stops running**.

The problem is concrete. In edge computing, the per-device operations cost dominates: install a terminal, install a process supervisor, configure start-on-boot, handle architecture differences, and keep a record of what was installed — every single device, by hand. FERC compresses that into **one install command**, and afterwards the node barely needs attention: terminal and file management can be handled directly by an administrator or by an agent.

Three defining traits:

1. **One-shot bootstrap** — FERC probes the platform itself, installs dependencies (such as SimpleWebShell), adds a process supervisor (DontCrack) when the system lacks native supervision, and finishes the node configuration.
2. **Not resident** — it is a CLI, not a daemon. It exits once it is done, so there is no "who supervises the supervisor" recursion.
3. **App-store style distribution** — components (built for all architectures by default) are pulled with `ferc install <component>` and removed with `ferc remove`.

### 2. Design decisions

#### Why it is not resident

Edge devices are short on memory and patience. If the running core were itself a daemon, operators would have to care about whether it is alive, how much its logs have grown, and how to stop it during upgrades. As a one-shot CLI:

- its crash is not a node failure, because it is not running on the node;
- upgrading means replacing a binary, not restarting a service;
- it needs no supervisor of its own, so there is no infinite supervision chain.

#### Why it checks the init system before deciding to install a supervisor

Supervision capability differs sharply across systems, and **"starts on boot" is not "restarts after a crash"**:

| Platform | Start on boot | Restart on crash | What FERC does |
|----------|---------------|------------------|----------------|
| systemd | yes | yes (`Restart=always`) | writes a unit, no extra supervisor |
| OpenRC | yes | yes (`supervise-daemon`) | writes an init script, no extra supervisor |
| launchd | yes | yes (`KeepAlive`) | writes a plist, no extra supervisor |
| sysvinit | yes | **no** | init script **plus automatic DontCrack**, which catches crashes |
| Windows Task Scheduler | yes | **no** | scheduled task **plus automatic DontCrack** |
| no init (minimal system / container) | no | no | DontCrack only |

This is where "install a process manager if the system does not have one" lands. The decision is made from two orthogonal capabilities in `internal/platform` (`Autostart()` and `Respawn()`), not from guessing by OS name.

#### Why every service gets its own control port

One DontCrack instance supervises exactly one process and owns one HTTP control port (11883 by default). If two services in a single `bootstrap` both need supervision, the second instance would fight the first for the port and fail to start — and DontCrack only logs a bind failure instead of exiting, so "the process is alive" is not proof of success. Therefore:

- the n-th supervised service takes `11883 + n`, increasing with install order;
- the assigned port is recorded as `control_port` in `ferc-state.json`, so uninstall can find the matching manager instance;
- install first probes the manager's control port, then requires `/healthz` to report a running child several times in a row; failing either is a hard error that reclaims the spawned manager and lets the transactional install roll back (otherwise you get a node that "installed fine" but crash-loops forever);
- `ferc remove` first asks the manager to stop its child (`/shutdown`), then terminates the manager itself, leaving no orphan holding the port. A child that already crashed is not reported as an uninstall failure.

#### Why installation is transactional

A half-finished install is the worst outcome: partial files on disk, a state file that does not know about them, and the next person left guessing. Every FERC step — download, verify, extract, pick the executable — happens inside a staging directory. Only after all steps succeed are files atomically moved into place; any failure rolls back both moved files and registered services, leaving the state file untouched.

### 3. Quick start

```bash
# 1. One command: probe platform -> install defaults -> register supervision
ferc bootstrap

# 2. Preview first, if you prefer
ferc bootstrap --dry-run

# 3. Install specific components
ferc install simple-web-shell mqtt-broker-core

# 4. Inspect / check / remove
ferc list
ferc status
ferc verify
ferc remove netmap
```

One-line install (Linux, root required to register services):

```bash
curl -fsSL https://github.com/FasterEdge/FasterEdgeRunningCore/releases/latest/download/ferc-linux-amd64.tar.gz \
  | tar xz -C /usr/local/bin ferc && ferc bootstrap
```

### 4. Subcommands

| Command | Purpose |
|---------|---------|
| `ferc bootstrap` | Install the default components (those marked `essential`) and register supervision |
| `ferc install <component...>` | Install on demand; `--all` installs everything |
| `ferc list` | List available components with host support and install status |
| `ferc status` | List installed components with version, supervisor and path |
| `ferc verify` | Check that installed files still exist and remain executable |
| `ferc remove <component...>` | Uninstall components and unregister their services |
| `ferc detect` | Print platform detection and the supervision verdict |

Common options:

| Option | Meaning |
|--------|---------|
| `--root <dir>` | Data root (default `/opt/FasterEdgeRunningCore`, `%ProgramData%\FasterEdgeRunningCore` on Windows) |
| `--dry-run` | Print the plan only; nothing is downloaded or written |
| `--json` | Machine-readable output |
| `--no-start` | Write files only, do not register services (common without root) |
| `--allow-conflicts` | Downgrade port/mutual-exclusion conflicts to warnings |
| `--catalog <file>` | Override the built-in component catalog |
| `--token <token>` | GitHub token (defaults to `GH_TOKEN` / `GITHUB_TOKEN`) |

Exit codes: `0` success, `1` execution failure or failed verification, `2` invalid arguments.

### 5. Data root layout

```
/opt/FasterEdgeRunningCore/
├── bin/                     component executables
├── logs/                    per-component log directories
├── ferc-state.json          install manifest (the only "memory")
└── .ferc-stage-*/           staging directories (cleaned up either way)
```

`ferc-state.json` records each component's name, version, path, arguments, supervisor and unit name. Because the running core is not resident, **this file is the node's install truth** — `status`, `verify` and `remove` all read from it.

### 6. Component catalog

The catalog is embedded in the binary (`internal/catalog/catalog.json`) and can be overridden with `--catalog`, so adding a component does not require rebuilding the running core.

| Component | Kind | Summary |
|-----------|------|---------|
| `dontcrack` | supervisor | Self-hosted process supervisor, installed when native supervision is missing |
| `simple-web-shell` | service | Web terminal and file manager (installed by `bootstrap`) |
| `mqtt-broker-core` | service | MQTT broker |
| `simple-time-service` | service | Time synchronisation service |
| `netmap` | service | Network topology |
| `fasteredge2api` | service | Cluster management HTTP API gateway |
| `proxyarea` | service | Protocol forwarding proxy |
| `rtsp2other` | service | RTSP stream forwarding |
| `dontcrack-manager` | service | Multi-supervisor manager |
| `fasteredge-doctor` | tool | Health diagnostics |
| `any2utf8` | tool | Encoding conversion |
| `any2pcd` | tool | Point-cloud conversion |
| `fasteredge-demo` | tool | Framework example |

Each entry declares its repository, artifact naming rule, unpacked executable name, supported architectures, default ports, dependencies and conflicts. Artifact names follow the existing release convention (`<stem>-<goos>-<goarch>.tar.gz|zip`) and are asserted one by one in `internal/catalog` tests, so a rename cannot silently break downloads.

### 7. Security boundaries

- **Extraction hardening**: absolute paths, `..` escapes, drive letters, colon-containing paths, symlinks and device files are rejected; per-file, total-size and entry-count limits block tar bombs.
- **Verification first**: when a release ships `SHA256SUMS.txt` / `checksums-sha1.txt`, verification is mandatory; a failed checksum-file download is a hard failure, because "could not fetch the checksum" must never degrade into "skipped verification". When no checksum file exists, the log says so plainly instead of pretending.
- **No supervision for tools**: `kind=tool` components never get a service registration — a one-shot tool that restarts forever is a bug, not a feature.
- **No supervisor for the supervisor**: the process manager marks itself `self`, so the supervision chain cannot form a cycle.

### 8. Build and test

```bash
go test ./...
go vet ./...
go build -o ferc ./cmd/ferc
```

Every platform-dependent decision is injected through `platform.Env`, and each init system's unit file is rendered by a pure function and asserted. Tests therefore need neither root nor a real `/etc`, and run on Windows, macOS and Linux alike.

### 9. Docker

```bash
docker build -t ferc .
docker run --rm -v /:/host ferc detect
```

The image is not resident either — it is only a portable environment that carries `ferc`.

### 10. License

Apache License 2.0, see `LICENSE`.
