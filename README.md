<div align="center">
  <img src="./Logo.png" alt="logo" width="100" />
  <h2>FasterEdgeRunningCore</h2>
  <h3>FERC —— FasterEdge 运行核</h3>
  <p>把边缘单设备的运维成本归为一行安装指令</p>
  <p>
    <a href="./README.md">中文</a> ·
    <a href="./README_en.md">English</a>
  </p>
</div>

### 一、这是什么

FERC（FasterEdge Running Core，运行核）是一个**不常驻**的单点式部署工具。各类操作系统装载它之后，即可自动初始化依赖（如 WebShell，以及系统没有内置进程管理器时自动装载一个用于进程保活，供后续 Agent 接管），完成节点的全部配置——可以全走默认一键完成。

之后此工具不做常驻进程，但作为命令行工具提供其他组件的直接拉取和装载，作用类似应用商店：各组件默认兼容全架构。终端与文件管理既可由管理员统一手动管理，也可由 Agent 统一管理。

一句话：**一行安装指令，之后几乎不用再管这个端。**

### 二、核心价值

- **一行指令完成节点配置**：`ferc bootstrap` 探测平台 → 装载默认组件 → 注册托管 → 完成。不需要 SSH 进去敲一串命令。
- **装完即走，不占资源**：运行核不是守护进程。它每次被调用做完一件事就退出，节点后续由系统 init 或已装载的 DontCrack 进程管理器托管。
- **应用商店式装载**：`ferc list` 看有什么，`ferc install <组件>` 装什么。组件清单内嵌在二进制里，也可用 `--catalog` 指向外部文件覆盖，新增组件不需要重新编译运行核。
- **不通外网也能装**：`ferc mirror export` 导出一份可整体搬迁的目录，U 盘直接拷、或由上游节点 `ferc mirror serve` 提供 HTTP 服务，下游全程不需要外网、也不需要任何凭据。
- **重启后自己回来**：装载成功即落盘开机自启（systemd / openrc / sysvinit / launchd / 计划任务 / autorun 六种机制），重启后 `ferc restore` 按 `ferc-state.json` 重放托管关系。
- **19 个目标平台**：linux ×14（含 armv6/armv7、loong64、mips×4、ppc64×2、riscv64、s390x）+ darwin ×2 + windows ×3。

### 三、快速开始

从 [Releases](https://github.com/FasterEdge/FasterEdgeRunningCore/releases) 下载对应平台的包，解压即得单个 `ferc` 可执行文件，无需安装：

| 平台 | 产物 |
|------|------|
| Linux（14 架构） | `ferc-linux-<arch>.tar.gz`，`<arch>` ∈ `386` `amd64` `armv6` `armv7` `arm64` `loong64` `mips` `mipsle` `mips64` `mips64le` `ppc64` `ppc64le` `riscv64` `s390x` |
| macOS | `ferc-darwin-amd64.tar.gz`、`ferc-darwin-arm64.tar.gz` |
| Windows | `ferc-windows-386.zip`、`ferc-windows-amd64.zip`、`ferc-windows-arm64.zip` |

每个 Release 同时附带 `checksums-md5.txt` 与 `checksums-sha1.txt`，可核对下载文件完整性。

```bash
# 1. 一键：探测平台 → 装默认组件 → 注册托管 → 完成节点配置
sudo ferc bootstrap

# 2. 只想先看看会做什么
ferc bootstrap --dry-run

# 3. 装指定组件（应用商店式）
ferc install simple-web-shell mqtt-broker-core

# 4. 查看 / 检查 / 卸载
ferc list
ferc status
ferc verify
ferc remove simple-web-shell
```

`bootstrap` 默认装载清单里标记 `essential` 的组件。若目标系统没有原生进程保活能力（systemd / openrc / launchd 之外），会**自动装载一个 DontCrack 进程管理器**用于保活，供后续 Agent 接管。

非 root 运行也能用：加 `--no-start` 只落盘不注册服务；装载后执行 `ferc boot enable` 补上开机自启。

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
| `ferc boot enable\|disable\|status` | 落盘 / 撤销 / 查看开机自启 |
| `ferc mirror export\|serve` | 导出 / 分享离线镜像（给不通外网的节点取包） |
| `ferc detect` | 打印平台探测结果与保活能力判定 |
| `ferc version` | 打印版本 |

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
| `--arch <a,b>` | `mirror export` 时限定只导出指定架构，可重复或逗号分隔 |
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

`ferc-state.json` 记录了每个组件的名字、版本、路径、启动参数、托管方、控制端口与单元名。运行核本身不常驻，因此**这个文件就是节点的安装事实**——`status`、`verify`、`remove`、`restore` 都以它为准。

### 六、组件清单

组件清单内嵌在二进制里（`internal/catalog/catalog.json`），也可用 `--catalog` 指向外部文件覆盖，因此新增组件不需要重新编译运行核。

| 组件 | 类型 | 默认端口 | 装载策略 | 说明 |
|------|------|---------|---------|------|
| `dontcrack` | supervisor | 11883 | 按需 | 自托管进程管理器，无原生保活能力时自动装载 |
| `simple-web-shell` | service | 8878 | `bootstrap` 默认 | 受密码保护的 Web 终端与文件管理 |
| `mqtt-broker-core` | service | 1883 | 按需 | 本地 MQTT 消息总线 |
| `simple-time-service` | service | 8080 | 按需 | UTC 时间与 NTP 对时 JSON 接口 |
| `netmap` | service | 8080 | 按需 | 带 Web 前端的网络拓扑管理器 |
| `fasteredge2api` | service | 8080 | 按需 | 节点装配 HTTP API 网关（拓扑、集群角色、令牌认证） |
| `proxyarea` | service | 8080 | 按需 | 协议转发与代理 |
| `rtsp2other` | service | 无 | 按需 | RTSP 视频流转换/转发 |
| `dontcrack-manager` | service | 11884 | 按需 | 多 DontCrack 进程的总管理器 |
| `fasteredge-doctor` | tool | 7080 | 按需 | 健康诊断 |
| `any2utf8` | tool | 无 | 按需 | 文本与文件转 UTF-8 |
| `any2pcd` | tool | 无 | 按需 | 点云文件转 PCD |
| `fasteredge-demo` | tool | 无 | 按需 | FasterEdge 框架示例节点 |

清单里每个组件声明了：产物仓库、产物命名规则、解包后的可执行文件名、支持的架构、默认端口、依赖与互斥关系。产物命名直接对齐各仓 release 的既有约定（`<stem>-<goos>-<goarch>.tar.gz|zip`），并在 `internal/catalog` 的测试里逐条断言，防止改名后静默下不到东西。

### 七、安全边界

- **解包防护**：拒绝绝对路径、`..` 逃逸、盘符、含冒号的路径、符号链接与设备文件；单文件、总量、条目数都有上限，挡 tar 炸弹。
- **校验优先**：release 若提供 `SHA256SUMS.txt` / `checksums-sha256.txt` / `checksums-sha1.txt`，强制校验；校验文件下载失败直接判失败，不会因为“拿不到校验文件”而跳过校验。没有校验文件时日志会明确写“本次未校验”，不假装校验过。
- **镜像自校验**：`mirror export` 给每个产物当场算 SHA256 写进镜像索引。原仓库有的发 checksums、有的根本不发，镜像这一层让“从邻居那儿拷来的包”也能核对。
- **不给工具装保活**：`kind=tool` 的组件不会注册任何服务——一次性工具跑完就该退出，给它注册重启只会造成死循环。
- **不给 supervisor 套 supervisor**：进程管理器自身标记为 `self`，避免保活链成环。
- **多服务端口隔离**：DontCrack 一个实例只托管一个进程、只占一个控制端口，第 n 个自托管服务用基准端口 + n，不会互相抢端口。
- **事务式安装**：每一步先在暂存目录完成“下载 → 校验 → 解包 → 挑出可执行文件”，全部成功才原子落到最终路径；任一步失败即回滚，不会留下半装状态。

### 八、不通外网的节点怎么取包

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

# 二级转发：上游节点自己也没外网时，先吃 U 盘再吐给现场
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

### 九、构建与测试

```bash
go test ./...
go vet ./...
go build -o ferc ./cmd/ferc
```

所有平台相关判断都通过 `platform.Env` 注入，各 init 系统的单元文件由纯函数渲染并断言，因此测试不需要 root、不需要真的去改 `/etc`，在 Windows/macOS/Linux 上都能跑。CI 额外跑 `go test -race` 做竞态检测。

发布矩阵覆盖 19 个 target：`linux/{386,amd64,armv6,armv7,arm64,loong64,mips,mipsle,mips64,mips64le,ppc64,ppc64le,riscv64,s390x}`、`darwin/{amd64,arm64}`、`windows/{386,amd64,arm64}`。其中 `armv6` 与 `armv7` 靠 `GOARM` 与 `//go:build arm.6` / `arm.7` 区分：`GOARM=6` 只置 `arm.6`，`GOARM=7` 同时置 `arm.6` 和 `arm.7`，因此 `arch_armv6.go` 用 `arm.6 && !arm.7`、`arch_armv7.go` 用 `arm.7`。

推送 `v*` 格式的 tag（例如 `v1.0.20261127`）即触发 `.github/workflows/release.yml`：19 路并行交叉编译 → 打包 → 生成 `checksums-md5.txt` 与 `checksums-sha1.txt` → 自动创建或更新 GitHub Release。版本号规则为 `1.0.YYYYMMDD`，记录在 `VERSION` 文件里。

```bash
git tag v1.0.20261127
git push origin v1.0.20261127
```

### 十、Docker

```bash
docker build -t ferc .
docker run --rm -v /:/host ferc detect
```

镜像本身不常驻，只是一个随身携带 `ferc` 的干净环境：既可以在容器里对宿主机做探测与装载，也可以作为 CI 里的发布工具。容器内保留 `ca-certificates` 与 `tar`，因此 `bootstrap` 能完整跑通。

### 十一、项目结构

```
.
├── cmd/ferc/main.go             CLI 入口
├── internal/
│   ├── cli/                     子命令分发、参数解析、输出
│   ├── catalog/                 组件清单（内嵌 JSON + 命名规则断言）
│   ├── platform/                平台探测：GOOS/GOARCH/init/权限
│   ├── fetch/                   GitHub release 客户端 + Source 接口
│   ├── install/                 事务式装载：下载 → 校验 → 解包 → 落盘
│   ├── resolve/                 依赖、互斥、端口冲突解析
│   ├── supervise/               托管注册：systemd/openrc/sysv/launchd/schtasks/dontcrack
│   ├── state/                   ferc-state.json 读写（原子写）
│   ├── mirror/                  离线镜像：导出 / 本地与 HTTP 读取 / 多源链
│   └── boot/                    开机自启方案渲染（六种机制，纯函数）
├── .github/workflows/
│   ├── ci.yml                   gofmt / vet / test / race / 19 路交叉编译
│   └── release.yml              交叉编译打包 + Release 发布
├── VERSION                      当前版本号（1.0.YYYYMMDD）
└── Dockerfile                   便携容器环境
```

### 十二、许可

Apache License 2.0，见 [LICENSE](./LICENSE)。

由 [FasterEdge](https://github.com/FasterEdge) 发行。
