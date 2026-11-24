<div align="center">
  <img src="./Logo.png" alt="logo" width="100" />
  <h2>FasterEdgeRunningCore</h2>
  <h3>FERC — FasterEdge Running Core</h3>
  <p>Reduce single-device edge operations to one install command</p>
  <p>
    <a href="./README.md">中文</a> ·
    <a href="./README_en.md">English</a>
  </p>
</div>

### 1. What this is

FERC (FasterEdge Running Core) is a **non-resident**, single-shot deployment tool. Load it onto any operating system and it initialises the dependencies automatically — a WebShell, plus a process supervisor when the system has none built in, for keep-alive and later Agent takeover — completing every piece of node configuration, all defaults, one command.

Afterwards the tool is not a resident process. It remains a command-line tool that pulls and loads other components directly, much like an app store: every component is compatible with all architectures by default. Terminal and file management can be handled by an administrator uniformly and manually, or by an Agent uniformly.

In one line: **one install command, and you almost never touch this endpoint again.**

### 2. Core value

- **One command configures a node**: `ferc bootstrap` detects the platform, installs the default components, registers supervision, and finishes. No SSH session, no chain of commands.
- **Leaves when done, costs nothing**: the running core is not a daemon. Every invocation does one thing and exits; the node is then supervised by the system init or by the installed DontCrack process manager.
- **App-store style loading**: `ferc list` shows what is available, `ferc install <component>` installs it. The component catalog is embedded in the binary and can be overridden with `--catalog`, so adding a component does not require recompiling the running core.
- **Works without internet**: `ferc mirror export` produces a relocatable directory — copy it on a USB stick, or let an upstream node serve it over HTTP with `ferc mirror serve`. The downstream node needs neither internet nor credentials.
- **Comes back after reboot**: a successful install writes the boot hook (systemd / openrc / sysvinit / launchd / Task Scheduler / autorun), and `ferc restore` replays supervision from `ferc-state.json` on the next boot.
- **19 target platforms**: linux ×14 (including armv6/armv7, loong64, mips ×4, ppc64 ×2, riscv64, s390x) + darwin ×2 + windows ×3.

### 3. Quick start

Download the package for your platform from [Releases](https://github.com/FasterEdge/FasterEdgeRunningCore/releases), unpack it, and you get a single `ferc` executable. No installation required:

| Platform | Artifact |
|----------|----------|
| Linux (14 architectures) | `ferc-linux-<arch>.tar.gz`, `<arch>` ∈ `386` `amd64` `armv6` `armv7` `arm64` `loong64` `mips` `mipsle` `mips64` `mips64le` `ppc64` `ppc64le` `riscv64` `s390x` |
| macOS | `ferc-darwin-amd64.tar.gz`, `ferc-darwin-arm64.tar.gz` |
| Windows | `ferc-windows-386.zip`, `ferc-windows-amd64.zip`, `ferc-windows-arm64.zip` |

Every release also ships `checksums-md5.txt` and `checksums-sha1.txt` so you can verify download integrity.

```bash
# 1. One shot: detect platform -> install defaults -> register supervision -> done
sudo ferc bootstrap

# 2. Only see what would happen
ferc bootstrap --dry-run

# 3. Install specific components (app-store style)
ferc install simple-web-shell mqtt-broker-core

# 4. Inspect / check / uninstall
ferc list
ferc status
ferc verify
ferc remove simple-web-shell
```

`bootstrap` installs the components marked `essential` in the catalog. If the target system has no native supervision (anything beyond systemd / openrc / launchd), it **automatically installs a DontCrack process manager** for keep-alive, ready for a later Agent takeover.

Running without root works too: add `--no-start` to write files without registering services, then run `ferc boot enable` afterwards to add the boot hook.

### 4. Subcommands

| Command | Purpose |
|---------|---------|
| `ferc bootstrap` | Install the default components (those marked `essential`) and register supervision |
| `ferc install <component...>` | Install on demand; `--all` installs everything |
| `ferc list` | List available components with host support and install status |
| `ferc status` | List installed components with version, supervisor and path |
| `ferc verify` | Check that installed files still exist and remain executable |
| `ferc remove <component...>` | Uninstall components and unregister their services |
| `ferc restore` | Replay supervision from `ferc-state.json` after a reboot (this is what the boot hook runs) |
| `ferc boot enable\|disable\|status` | Write / remove / inspect the boot hook |
| `ferc mirror export\|serve` | Export / share an offline mirror (for nodes without internet) |
| `ferc detect` | Print platform detection and the supervision verdict |
| `ferc version` | Print the version |

Common options:

| Option | Meaning |
|--------|---------|
| `--root <dir>` | Data root (default `/opt/FasterEdgeRunningCore`, `%ProgramData%\FasterEdgeRunningCore` on Windows) |
| `--dry-run` | Print the plan only; nothing is downloaded or written |
| `--json` | Machine-readable output |
| `--no-start` | Write files only, do not register services (common without root) |
| `--no-boot` | Install successfully but skip writing the boot hook |
| `--allow-conflicts` | Downgrade port/mutual-exclusion conflicts to warnings |
| `--catalog <file>` | Override the built-in component catalog |
| `--from <dir\|url>` | Fetch packages from a mirror (local directory or HTTP mirror) instead of the internet |
| `--fallback-github` | Fall back to GitHub for packages missing from the mirror |
| `--arch <a,b>` | Restrict `mirror export` to the given architectures; repeatable or comma-separated |
| `--token <token>` | GitHub token (defaults to `GH_TOKEN` / `GITHUB_TOKEN`) |

Exit codes: `0` success, `1` execution failure or failed verification, `2` invalid arguments.

### 5. Data root layout

```
/opt/FasterEdgeRunningCore/
├── bin/                     component executables
│   ├── dontcrack-manylinux
│   └── simple-web-shell
├── logs/                    per-component log directories
├── ferc-state.json          the installed manifest (the only "memory")
└── .ferc-stage-*/           staging directories during install (cleaned on success and failure)
```

`ferc-state.json` records each component's name, version, path, launch arguments, supervisor, control port and unit name. The running core is not resident, so **this file is the installation fact of the node** — `status`, `verify`, `remove` and `restore` all read it.

### 6. Component catalog

The catalog is embedded in the binary (`internal/catalog/catalog.json`) and can be overridden with `--catalog`, so adding a component does not require recompiling the running core.

| Component | Kind | Default port | Install policy | Notes |
|-----------|------|--------------|----------------|-------|
| `dontcrack` | supervisor | 11883 | on demand | Self-hosted process manager, installed automatically when no native supervision exists |
| `simple-web-shell` | service | 8878 | `bootstrap` default | Password-protected web terminal and file management |
| `mqtt-broker-core` | service | 1883 | on demand | Local MQTT message bus |
| `simple-time-service` | service | 8080 | on demand | UTC time and NTP sync JSON API |
| `netmap` | service | 8080 | on demand | Web-fronted network topology manager |
| `fasteredge2api` | service | 8080 | on demand | Node assembly HTTP API gateway (topology, cluster roles, token auth) |
| `proxyarea` | service | 8080 | on demand | Protocol forwarding and proxy |
| `rtsp2other` | service | none | on demand | RTSP stream conversion/forwarding |
| `dontcrack-manager` | service | 11884 | on demand | Manager for multiple DontCrack processes |
| `fasteredge-doctor` | tool | 7080 | on demand | Health diagnostics |
| `any2utf8` | tool | none | on demand | Text and file conversion to UTF-8 |
| `any2pcd` | tool | none | on demand | Point-cloud file conversion to PCD |
| `fasteredge-demo` | tool | none | on demand | FasterEdge framework example node |

Each entry declares its repository, artifact naming rule, unpacked executable name, supported architectures, default ports, dependencies and conflicts. Artifact names follow the existing release convention (`<stem>-<goos>-<goarch>.tar.gz|zip`) and are asserted one by one in `internal/catalog` tests, so a rename cannot silently break downloads.

### 7. Security boundaries

- **Extraction hardening**: absolute paths, `..` escapes, drive letters, colon-containing paths, symlinks and device files are rejected; per-file, total-size and entry-count limits block tar bombs.
- **Verification first**: when a release ships `SHA256SUMS.txt` / `checksums-sha256.txt` / `checksums-sha1.txt`, verification is mandatory; a failed checksum-file download is a hard failure, because "could not fetch the checksum" must never degrade into "skipped verification". When no checksum file exists, the log says so plainly instead of pretending.
- **Mirrors self-checksum**: `mirror export` computes a SHA256 for every artifact on the spot and writes it into the mirror index. Upstream repositories publish checksums inconsistently — some do, some do not at all — so this layer lets a node verify a package it received from a neighbour.
- **No supervision for tools**: components of `kind=tool` never get a service registration — a one-shot tool that restarts forever is a bug, not a feature.
- **No supervisor for the supervisor**: the process manager marks itself `self`, so the supervision chain cannot loop.
- **Per-service port isolation**: one DontCrack instance supervises exactly one process and holds exactly one control port, so the n-th self-hosted service uses base port + n and never fights over a port.
- **Transactional install**: every step completes "download → verify → unpack → extract executable" in a staging directory first, and only then is the final path written atomically. Any failure rolls back, so no half-installed state is left behind.

### 8. Nodes without internet access

A mirror directory records only `repo + tag + filename`, never an absolute address, so the client just swaps the base. Offline means copying the directory to a USB stick; LAN means letting one internet-capable node serve that directory:

```bash
# 1. Export on a node that has internet (narrow it with --arch amd64,arm64)
ferc mirror export --dir /media/usb/ferc-mirror

# 2a. USB stick: copy the directory to the target
# 2b. LAN: serve it from the online node
ferc mirror serve --dir /media/usb/ferc-mirror          # 0.0.0.0:8090 by default

# 3. Install on the target — no internet, no downstream credentials
ferc install simple-web-shell --from /media/usb/ferc-mirror
ferc install simple-web-shell --from http://10.0.0.9:8090

# Two-hop: an upstream node with no internet itself, re-exporting from a USB stick
ferc mirror export --dir /srv/ferc-lan --from /media/usb/ferc-mirror
```

`--from` coexists with GitHub: add `--fallback-github` and packages missing from the mirror fall back to GitHub, while packages present in the mirror are preferred.

Mirror layout:

```
<base>/
├── ferc-mirror.json                                    index: repo -> latest tag
└── repos/<owner>/<name>/releases/<tag>/
    ├── release.json                                    artifact list (each with SHA256)
    └── <stem>-<goos>-<goarch>.tar.gz|zip
```

### 9. Build and test

```bash
go test ./...
go vet ./...
go build -o ferc ./cmd/ferc
```

Every platform-dependent decision is injected through `platform.Env`, and each init system's unit file is rendered by a pure function and asserted. Tests therefore need neither root nor a real `/etc`, and run on Windows, macOS and Linux alike. CI additionally runs `go test -race` for race detection.

The release matrix covers 19 targets: `linux/{386,amd64,armv6,armv7,arm64,loong64,mips,mipsle,mips64,mips64le,ppc64,ppc64le,riscv64,s390x}`, `darwin/{amd64,arm64}`, `windows/{386,amd64,arm64}`. `armv6` and `armv7` are distinguished by `GOARM` together with `//go:build arm.6` / `arm.7`: `GOARM=6` sets only `arm.6`, `GOARM=7` sets both `arm.6` and `arm.7`, hence `arch_armv6.go` uses `arm.6 && !arm.7` and `arch_armv7.go` uses `arm.7`.

Pushing a `v*` tag (for example `v1.0.20261127`) triggers `.github/workflows/release.yml`: 19 parallel cross-compiles → packaging → generating `checksums-md5.txt` and `checksums-sha1.txt` → creating or updating the GitHub Release automatically. The version scheme is `1.0.YYYYMMDD`, recorded in the `VERSION` file.

```bash
git tag v1.0.20261127
git push origin v1.0.20261127
```

### 10. Docker

```bash
docker build -t ferc .
docker run --rm -v /:/host ferc detect
```

The image is not resident either — it is only a portable environment that carries `ferc`: useful for detecting and installing on the host from inside a container, and as a release tool in CI. `ca-certificates` and `tar` are kept inside the image so `bootstrap` runs end to end.

Docker Hub is often unreachable or slow from some networks, so the base images go through a registry mirror selected by the `MIRROR` build argument — no Dockerfile edit required:

```bash
# Use 1ms.run (note: this site may require login; anonymous pulls return 401)
docker build --build-arg MIRROR=docker.1ms.run/ -t ferc .

# Or another site that allows anonymous pulls
docker build --build-arg MIRROR=docker.1panel.live/ -t ferc .
```

Omitting `MIRROR` pulls from Docker Hub, exactly as before.

### 11. Project layout

```
.
├── cmd/ferc/main.go             CLI entry point
├── internal/
│   ├── cli/                     subcommand dispatch, flag parsing, output
│   ├── catalog/                 component catalog (embedded JSON + naming assertions)
│   ├── platform/                platform detection: GOOS/GOARCH/init/privileges
│   ├── fetch/                   GitHub release client + Source interface
│   ├── install/                 transactional install: download -> verify -> unpack -> place
│   ├── resolve/                 dependency, exclusion and port-conflict resolution
│   ├── supervise/               supervision registration: systemd/openrc/sysv/launchd/schtasks/dontcrack
│   ├── state/                   ferc-state.json read/write (atomic)
│   ├── mirror/                  offline mirror: export / local and HTTP reads / multi-source chain
│   └── boot/                    boot hook rendering (six mechanisms, pure functions)
├── .github/workflows/
│   ├── ci.yml                   gofmt / vet / test / race / 19 cross-compiles
│   └── release.yml              cross-compile packaging + release publishing
├── VERSION                      current version (1.0.YYYYMMDD)
└── Dockerfile                   portable container environment
```

### 12. License

Apache License 2.0, see [LICENSE](./LICENSE).

Published by [FasterEdge](https://github.com/FasterEdge).
