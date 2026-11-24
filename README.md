<div align="center">
  <img src="https://avatars.githubusercontent.com/u/245985800?s=200&v=4" alt="logo" width="100" />
  <h2>FasterEdgeRunningCore</h2>
  <h3>FERC - 单点式通用统一管理部署工具</h3>
</div>

### 一、这是什么

`FERC`（`ferc`）是 FasterEdge 生态的**运行核**：在任意操作系统上装一次，让它自己把节点的依赖配齐，然后**不再常驻**。

它要解决的问题很具体：边缘计算场景里单台设备的运维成本极高——装终端、装进程管理器、配开机自启、处理架构差异、记录装了什么，每台都要人肉来一遍。FERC 的目标是把这套动作压缩成**一行安装指令**，并且此后这台端几乎不用再管：终端与文件管理可以直接由管理员手工操作，也可以交给 Agent 统一管理。

一句话概括它的三个特征：

1. **一次性引导**：运行核自己探测平台、装载依赖（如 SimpleWebShell）、必要时自动补一个进程管理器（DontCrack），把节点配置完成。
2. **不常驻**：它是个命令行工具，装完就退出，没有自己的守护进程——节点上不会有“为了保活 FERC 而再需要一个保活工具”的套娃。
3. **应用商店式分发**：各组件（默认兼容全架构）通过 `ferc install <组件>` 直接拉取装载，也可以 `ferc remove` 卸掉。

### 二、设计取舍

#### 为什么“不常驻”

边缘设备最缺的是内存与心智负担。如果运行核自己也是一个常驻服务，运维者就要额外关心它是否活着、日志涨了多少、升级时怎么停机。把它做成一次性 CLI 之后：

- 它的崩溃不再是节点故障，因为它根本不在节点上跑；
- 升级就是换个二进制，不涉及重启服务；
- 它不需要自己的进程管理器，也就没有“无限保活递归”。

#### 为什么“先判 init 再决定要不要装进程管理器”

不同系统的保活能力差异极大，而且**“能开机自启”不等于“崩溃后能被拉起”**：

| 平台 | 开机自启 | 崩溃拉起 | FERC 的动作 |
|------|---------|---------|------------|
| systemd | 是 | 是（`Restart=always`） | 直接写 unit，不装额外管理器 |
| OpenRC | 是 | 是（`supervise-daemon`） | 写 init 脚本，不装额外管理器 |
| launchd | 是 | 是（`KeepAlive`） | 写 plist，不装额外管理器 |
| sysvinit | 是 | **否** | 写 init 脚本 + **自动装 DontCrack** 兜住崩溃 |
| Windows 计划任务 | 是 | **否** | 注册计划任务 + **自动装 DontCrack** 兜住崩溃 |
| 无 init（精简系统 / 容器） | 否 | 否 | 仅靠 DontCrack 托管 |

这正是“如果系统没有内置进程管理器就自动装载一个”这条需求的落点。判定依据是 `internal/platform` 里的 `Autostart()` 与 `Respawn()` 两个正交能力，而不是靠系统名字猜。

#### 为什么要给每个服务分开控制端口

DontCrack 一个实例只托管一个进程、只占一个 HTTP 控制端口（默认 11883）。一次 `bootstrap` 里若有两个服务都要它托管，第二个实例就会去抢第一个的端口、启动即失败——而 DontCrack 端口绑不上时只打日志不退出，光看“进程还活着”会误判成功。因此：

- 第 n 个被托管服务拿 `11883 + n`，端口随装载顺序递增；
- 端口分配结果写进 `ferc-state.json` 的 `control_port`，卸载时按它找到对应的管理器实例；
- 装载时先探管理器控制端口，再要求 `/healthz` 连续多次报“子进程在跑”；任一不过就硬失败并回收已派生的管理器，让事务式安装连带回滚（否则会留下“装好了但一直崩溃重启”的节点）；
- `ferc remove` 会先让管理器停掉被托管的子进程（`/shutdown`），再结束管理器本身，不留孤儿进程占着端口。子进程早就崩了也不会被误报成卸载失败。

#### 为什么要事务式安装

装到一半失败最糟糕：磁盘上有半份文件、状态文件却不知道它的存在，下一个接手的人只能猜。FERC 的每一步都是「下载 → 校验 → 解包 → 挑出可执行文件」全部在暂存目录里完成，全部成功后才原子移动到最终位置；任一步失败则回滚已移动的文件与已注册的服务，状态文件保持原样。

### 三、快速开始

```bash
# 1. 一键：探测平台 → 装默认组件 → 注册托管 → 完成节点配置
ferc bootstrap

# 2. 只想先看看会做什么
ferc bootstrap --dry-run

# 3. 装指定组件（应用商店式）
ferc install simple-web-shell mqtt-broker-core

# 4. 查看 / 检查 / 卸载
ferc list
ferc status
ferc verify
ferc remove netmap
```

一行安装（Linux，需要 root 才能注册服务）：

```bash
curl -fsSL https://github.com/FasterEdge/FasterEdgeRunningCore/releases/latest/download/ferc-linux-amd64.tar.gz \
  | tar xz -C /usr/local/bin ferc && ferc bootstrap
```

### 四、子命令

| 命令 | 作用 |
|------|------|
| `ferc bootstrap` | 一键装载默认组件（清单里标记 `essential` 的）并完成托管 |
| `ferc install <组件...>` | 按需装载；`--all` 装全部 |
| `ferc list` | 列出可装载组件，标注本机是否支持、是否已装 |
| `ferc status` | 列出本机已装载组件、版本、托管方、路径 |
| `ferc verify` | 检查已装组件的文件是否还在、是否可执行 |
| `ferc remove <组件...>` | 卸载组件并撤销服务注册 |
| `ferc restore` | 重启后按 `ferc-state.json` 重放托管关系（开机自启就是调它） |
| `ferc boot enable\|disable\|status` | 落盘 / 撤销 / 查看开机自启（systemd、openrc、sysvinit、launchd、计划任务、autorun） |
| `ferc mirror export\|serve` | 导出一份可整体搬迁的离线镜像目录 / 把它挂在 HTTP 上给内网节点取包 |
| `ferc detect` | 打印平台探测结果与保活能力判定 |

通用选项：

| 选项 | 说明 |
|------|------|
| `--root <dir>` | 数据根目录（默认 `/opt/FasterEdgeRunningCore`，Windows 为 `%ProgramData%\FasterEdgeRunningCore`） |
| `--dry-run` | 只打印计划，不下载不落盘 |
| `--json` | 机器可读输出 |
| `--no-start` | 只落盘，不注册服务（非 root 场景常用） |
| `--no-boot` | 装载成功但不落盘开机自启 |
| `--allow-conflicts` | 把端口/互斥冲突降级为告警 |
| `--catalog <file>` | 用外部清单覆盖内置清单 |
| `--from <dir\|url>` | 从镜像取包（本地目录或 HTTP 镜像），不访问外网 |
| `--fallback-github` | 镜像里缺某个包时回落 GitHub |
| `--token <token>` | GitHub token（默认取 `GH_TOKEN` / `GITHUB_TOKEN`） |

退出码：`0` 成功，`1` 执行失败或检查不通过，`2` 参数错误。

### 五、数据根目录布局

```
/opt/FasterEdgeRunningCore/
├── bin/                     各组件可执行文件
│   ├── dontcrack-manylinux
│   └── simple-web-shell
├── logs/                    各组件的日志目录
├── ferc-state.json          已装载清单（唯一的“记忆”）
└── .ferc-stage-*/           安装期间的暂存目录（成功/失败都会清理）
```

`ferc-state.json` 记录了每个组件的名字、版本、路径、启动参数、托管方与单元名。运行核本身不常驻，因此**这个文件就是节点的安装事实**——`status`、`verify`、`remove` 都以它为准。

### 六、组件清单

组件清单内嵌在二进制里（`internal/catalog/catalog.json`），也可用 `--catalog` 指向外部文件覆盖，因此新增组件不需要重新编译运行核。

已收录的组件：

| 组件 | 类型 | 说明 |
|------|------|------|
| `dontcrack` | supervisor | 自托管进程管理器，无原生保活能力时自动装载 |
| `simple-web-shell` | service | Web 终端与文件管理（`bootstrap` 默认装载） |
| `mqtt-broker-core` | service | MQTT broker |
| `simple-time-service` | service | 对时服务 |
| `netmap` | service | 网络拓扑 |
| `fasteredge2api` | service | 集群管理 HTTP API 网关 |
| `proxyarea` | service | 协议转发代理 |
| `rtsp2other` | service | RTSP 流转发 |
| `dontcrack-manager` | service | 多进程总管 |
| `fasteredge-doctor` | tool | 健康诊断 |
| `any2utf8` | tool | 编码转换 |
| `any2pcd` | tool | 点云转换 |
| `fasteredge-demo` | tool | 框架示例 |

清单里每个组件声明了：产物仓库、产物命名规则、解包后的可执行文件名、支持的架构、默认端口、依赖与互斥关系。产物命名直接对齐各仓 release 的既有约定（`<stem>-<goos>-<goarch>.tar.gz|zip`），并在 `internal/catalog` 的测试里逐条断言，防止改名后静默下不到东西。

### 七、安全边界

- **解包防护**：拒绝绝对路径、`..` 逃逸、盘符、含冒号的路径、符号链接与设备文件；单文件、总量、条目数都有上限，挡 tar 炸弹。
- **校验优先**：release 若提供 `SHA256SUMS.txt` / `checksums-sha1.txt`，强制校验；校验文件下载失败直接判失败，不会因为“拿不到校验文件”而跳过校验。没有校验文件时日志会明确写“本次未校验”，不假装校验过。
- **不给工具装保活**：`kind=tool` 的组件不会注册任何服务——一次性工具跑完就该退出，给它注册重启只会造成死循环。
- **不给 supervisor 套 supervisor**：进程管理器自身标记为 `self`，避免保活链成环。
- **镜像自校验**：`mirror export` 给每个产物当场算 SHA256 写进镜像索引。原仓库有的发 checksums、有的根本不发，镜像这一层让“从邻居那儿拷来的包”也能核对，且索引里不存绝对 URL——整个目录拷到 U 盘依然可用。

### 七·五、不通外网的节点怎么取包

镜像目录只记「仓库 + tag + 文件名」，不记绝对地址，客户端只换基址。因此离线场景就是把目录拷到 U 盘，局域网场景就是让一台能上网的节点把目录挂起来：

```bash
# 1. 在上网节点导出（只导本集群需要的架构可以 --arch amd64,arm64）
ferc mirror export --dir /media/usb/ferc-mirror

# 2a. U 盘：把目录直接拷到目标机
# 2b. 局域网：在上网节点起 HTTP 服务
ferc mirror serve --dir /media/usb/ferc-mirror          # 默认 0.0.0.0:8090

# 3. 在目标节点装载，全程不需要外网、也不需要下游有任何凭据
ferc install simple-web-shell --from /media/usb/ferc-mirror
ferc install simple-web-shell --from http://10.0.0.9:8090

# 二级转发：上游自己也没外网时，先吃 U 盘再吐给现场
ferc mirror export --dir /srv/ferc-lan --from /media/usb/ferc-mirror
```

`--from` 与 GitHub 可以并存：加 `--fallback-github` 后，镜像里缺的包会回落到 GitHub，镜像里有的优先走镜像。

镜像目录布局：

```
<base>/
├── ferc-mirror.json                                    总目录：仓库 → 最新 tag
└── repos/<owner>/<name>/releases/<tag>/
    ├── release.json                                    产物清单（含各自 SHA256）
    └── <stem>-<goos>-<goarch>.tar.gz|zip
```

### 八、构建与测试

```bash
go test ./...
go vet ./...
go build -o ferc ./cmd/ferc
```

所有平台相关判断都通过 `platform.Env` 注入，各 init 系统的单元文件由纯函数渲染并断言，因此测试不需要 root、不需要真的去改 `/etc`，在 Windows/macOS/Linux 上都能跑。

发布矩阵覆盖 19 个 target：`linux/{386,amd64,armv6,armv7,arm64,loong64,mips,mipsle,mips64,mips64le,ppc64,ppc64le,riscv64,s390x}`、`darwin/{amd64,arm64}`、`windows/{386,amd64,arm64}`。其中 `armv6` 与 `armv7` 靠 `GOARM` 与 `//go:build arm.6` / `arm.7` 区分：`GOARM=6` 只置 `arm.6`，`GOARM=7` 同时置 `arm.6` 和 `arm.7`，因此 `arch_armv6.go` 用 `arm.6 && !arm.7`、`arch_armv7.go` 用 `arm.7`。

### 九、Docker

```bash
docker build -t ferc .
docker run --rm -v /:/host ferc detect
```

镜像里同样不常驻——容器只是提供一个带 `ferc` 的便携执行环境。

### 十、许可

Apache License 2.0，见 `LICENSE`。
