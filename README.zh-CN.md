# Longarm

**给你的编程智能体一只长臂。**（Give your coding agents a long arm.）Longarm
把你身边闲置的机器（桌下的工作站、一台旧笔记本、一台云主机）汇聚成一个统一的
worker 池，Claude Code、Copilot CLI 或者你自己都可以在上面运行命令、构建、
测试套件乃至完整的智能体会话——从任何一台笔记本出发，穿透任何 NAT。

[English](README.md) · [使用参考](docs/usage.md) ·
[设计文档](docs/design.md) · [CI/CD](docs/ci-cd.md)

## 为什么需要它

编程智能体很擅长开始干活，却很不擅长等活干完。一次 `pnpm test` 或一次 Rust
构建就能把笔记本的 CPU 占满，再开一个智能体会话负载翻倍，三个并行的子智能体
更是让风扇狂转、电池告急。与此同时，家里那台 32 核的机器和云上的那台虚拟机却
在闲着。

Longarm 让智能体（或者你）可以说一句 *“去那边跑”*：

- **卸载重活**：构建、测试套件、`claude -p` / `copilot -p` 会话都在 worker
  上运行，你的笔记本保持凉爽安静。
- **并行扇出**：每个并行子智能体都能分到自己的 worker 或 worktree，由一个
  知道哪台机器还有空闲内存的调度器来分配。
- **什么都扛得住**：任务在 worker 上以脱离（detached）方式运行。合上笔记本、
  切换网络、重启 worker 守护进程——任务照跑不误，你可以随时重新连接查看输出。
- **哪里都能连**：位于家用路由器和运营商级 NAT（CGNAT）之后的 worker，可以
  通过打洞直连；打洞失败时则走中继。

## 功能特性

- **以服务形式运行的 worker**——`longarmd` 以 Windows 服务、systemd unit 或
  launchd 守护进程的形式，在普通用户账户下运行（这样任务能找到该用户的 git、
  node 和智能体配置）。
- **重启不丢的任务**——每个任务和会话都运行在各自的小型 shim 进程中；守护进程
  崩溃或更新都不会杀掉它们。日志可以从任意字节偏移处开始流式读取，退出码会被
  原样回传。
- **交互式 PTY 会话**——类似 tmux 的 `longarm attach`：命名会话、多客户端同时
  连接，用 `Ctrl-] d` 脱离，重新连接时回放历史输出。
- **端口转发**——`longarm forward worker-1 3000` 把 worker 上的开发服务器
  映射到你的 `localhost`。
- **代码同步**——`longarm sync .` 把你的工作副本（提交、未提交的修改、未跟踪
  的文件）镜像到 worker 上的一个 git worktree；`longarm run --repo .` 则为
  任务创建一个全新的 worktree。
- **内置 sshd**——`ssh`、`scp`、`sftp` 和 VS Code Remote-SSH 都走同一条 mTLS
  通道，无需管理 SSH 密钥。
- **MCP 服务器**——`longarm mcp` 为 Claude Code 和 Copilot CLI 提供
  `fleet_status`、`fleet_exec`、`fleet_job_submit`、`fleet_job_wait`、
  `fleet_sync`、`fleet_forward` 等工具。密钥按名称传递，从不经过模型。
- **带内存门槛的调度器**——`longarm run -- make` 会挑选负载最低、且有足够空闲
  内存和 CPU 余量的 worker，按任务类型预留内存，优先选择已有你仓库的 worker，
  没有合适的机器时则排队等待。
- **Hub**——`longarm-hub` 是控制平面：一次性加入码、用于签发 mTLS 证书的内置
  CA（自动续期、可吊销）、节点注册表、全集群任务索引、审计日志、工具链配置、
  安装脚本。它从不处于数据平面上：hub 宕机时，worker 照常工作，客户端直接与
  它们通信。
- **三种传输方式竞速**——每次连接都会同时拨号所有链路，最先完成 mTLS 握手的
  胜出：
  - 普通 **TCP**（局域网、VPN、你自己的 tailnet），
  - **Tailscale**，通过内嵌的 `tsnet` 节点（Tailscale SaaS 或自托管的
    Headscale；hub 签发带标签的一次性密钥，无需安装 `tailscaled`），
  - **longnet**，Longarm 自研的 QUIC 传输，内置 NAT 类型检测、打洞，以及集成
    在 hub 中的中继兜底（UDP 或基于 TCP 的 TLS）。
- **托盘应用与 Web 面板**——`longarm-tray` 实时显示每个 worker 的 CPU/内存、
  你到每个 worker 的链路（直连或中继、RTT）、你的任务和端口转发；hub 的 Web
  面板（TOTP 登录）用于管理节点、任务、加入码和用户。
- **签名自更新**——发布版本使用 minisign 签名；worker 按金丝雀方式先行更新，
  对新版本做健康检查，失败时自行回滚。
- **安装包**——MSI（Windows）、deb/rpm（Linux）、pkg 和 Homebrew cask
  （macOS），hub 另有 Docker 镜像和 deb 包，并由 hub 提供一行命令安装脚本。

| Hub Web 面板 | 托盘弹窗 |
|---|---|
| ![Hub 面板的节点页](docs/ui-screenshots/panel-nodes.png) | ![longarm-tray 的 Workers 标签页](docs/ui-screenshots/tray-workers.png) |

## 架构

```
  laptop (client)                       hub (small cloud VM)
 ┌───────────────────────┐            ┌──────────────────────────────┐
 │ Claude Code / Copilot │            │ longarm-hub                  │
 │   └─ longarm mcp      │  gRPC/mTLS │  join codes · CA · registry  │
 │ longarm CLI           │◄──────────►│  scheduler · job index       │
 │ longarm daemon (pool, │  (control) │  web panel · release store   │
 │   forwards, latency)  │            │  longnet relay + STUN        │
 │ longarm-tray          │            └──────────────▲───────────────┘
 └──────────┬────────────┘                           │ heartbeats, metrics,
            │                                        │ job reports (control)
            │  data plane: exec, logs, PTY, files,   │
            │  forwards - raced over three legs:     │
            │    tcp · tailscale (tsnet) · longnet   │
            │    (direct after hole punching,        │
            │     else via the relay)                │
            ▼                                        │
 ┌─────────────────────────────────────────────────────────────────┐
 │ workers: longarmd (service)  worker-1 (Windows)  worker-2 (Linux)│
 │   shim per job/session · git mirrors + worktrees · built-in sshd │
 └─────────────────────────────────────────────────────────────────┘
```

Hub 只负责协调。命令、输出、文件和终端都在客户端与 worker 之间直接传输（或经
中继传输，并使用同一套 mTLS 证书做端到端加密）。

## 快速上手

你需要一个处处可达的 hub（一台有公网 IP 的小型虚拟机，开放端口 7443/tcp+udp
和 7444/udp）、一台或多台 worker，以及一个客户端。各平台的二进制文件见
[releases 页面](https://github.com/lubobill1990/longarm/releases)；
也可以用 Go ≥ 1.27.3 从源码构建：`go build -o bin/ ./cmd/...`。

**1. Hub**——一条命令部署到 Azure（容器实例，约 44 美元/月；详见
[deploy/azure](deploy/azure/README.md)），完成后会打印第 2 步要用的加入命令：

```sh
az login && cd deploy/azure && ./deploy.sh --name myhub --location eastasia   # 或 deploy.ps1，或：
```

[![Deploy to Azure](https://aka.ms/deploytoazurebutton)](https://portal.azure.com/#create/Microsoft.Template/uri/https%3A%2F%2Fraw.githubusercontent.com%2Flubobill1990%2Flongarm%2Fmain%2Fdeploy%2Fazure%2Fazuredeploy.json)

或者部署在任意有公网 IP 的主机上（详见 [deploy/hub](deploy/hub/README.md)）：

```sh
longarm-hub --data-dir /var/lib/longarm-hub ca init        # 集群的 CA
longarm-hub --data-dir /var/lib/longarm-hub serve &        # :7443 gRPC + 中继，127.0.0.1:7480 面板
longarm-hub --data-dir /var/lib/longarm-hub ca show        # CA 指纹
longarm-hub --data-dir /var/lib/longarm-hub join-code --role admin
```

**2. 你的笔记本**（客户端 + 管理员）：

```sh
longarm join hub.example.com:7443 <admin-code> --ca-fingerprint <fp>
longarm admin join-code --role worker          # 为每台 worker 生成一个一次性加入码
```

**3. 一台 worker**——注册、审批、以服务形式运行：

```sh
longarmd join hub.example.com:7443 <worker-code> --ca-fingerprint <fp> --name worker-1
longarm admin approve worker-1                 # 在笔记本上执行（或在 hub 上开启 auto_approve）
sudo longarmd service install --user alice      # Windows（管理员权限）：longarmd service install --user .\alice
longarmd service start
```

也可以使用 hub 提供的一行命令：`curl -fsSL https://hub.example.com/install.sh | sh -s -- --join <code>`
（Linux/macOS），`$env:LONGARM_JOIN='<code>'; irm https://hub.example.com/install.ps1 | iex`
（Windows）。

**4. 开始使用：**

```sh
longarm ls                                     # worker、负载、智能体会话
longarm exec worker-1 -- git --version         # 短命令，退出码原样回传
longarm run --repo . -f -- pnpm test           # 在全新 worktree 中调度任务，并跟踪其输出
longarm attach worker-1 -s agent -- claude     # 可随时脱离的交互式智能体会话
longarm forward worker-1 3000                  # worker 的开发服务器映射到 localhost:3000
```

**5. 交给你的智能体：**

```sh
claude mcp add longarm -- longarm mcp
# Copilot CLI：~/.copilot/mcp-config.json
#   {"mcpServers": {"longarm": {"type": "local", "command": "longarm", "args": ["mcp"], "tools": ["*"]}}}
```

然后直接提问：*“在集群上跑一遍完整测试套件，告诉我哪些失败了”*，或者
*“在不同的 worker 上启动三个子智能体，每个失败的包分配一个”*。

其他内容——不依赖 hub 的 worker、Tailscale/Headscale、中继、托盘应用、自更新、
安装包、防火墙——都在[使用参考](docs/usage.md)中。

## 安全模型

- **全程 mTLS。** 每个节点都持有一张由集群自有 CA 签发的证书，身份为
  `longarm://<role>/<name>`（角色：admin、client、worker、hub、relay、
  release）。Worker 按角色对每一次调用进行授权；数据平面上不存在密码或共享
  密钥。
- **注册**使用短时效的一次性加入码，并固定（pin）CA 指纹；除非 hub 开启了自动
  审批，新的 worker 需要等待审批。证书自动续期且可吊销；吊销列表会随下一次心跳
  下发到每台 worker。
- **Hub 永远看不到你的数据。** 它负责协调，但不在数据路径上；中继转发的是不透明
  的、端到端加密的数据包。
- **任务以 worker 的服务账户身份运行**，而不是 SYSTEM/root（除非你明确这样
  选择）。请把每个拥有 `client` 角色的客户端都视为能在其可达的 worker 上执行
  任意代码——这正是这个工具的用途。不同的信任域请使用不同的集群（CA）。
- **密钥**（`--secret-env`）通过 mTLS 传输，只存在于任务的内存和环境变量中，
  并会在日志中被脱敏。MCP 服务器只转发运维人员加入白名单的密钥
  （`LONGARM_MCP_SECRETS`）。
- **本地控制 API** 使用命名管道 / unix socket，仅所有者可读。
- **更新**经过签名（minisign），并用内置于二进制文件中的公钥校验；CI 的凭据
  可以发布和推送更新，但无法降级。

漏洞报告方式见 [SECURITY.md](SECURITY.md)。

## 现状与限制

Longarm 还很年轻。它已在一个小型混合集群（Windows、Linux、macOS worker）上
日常承载智能体工作负载，并在三个平台上都有完善的端到端测试套件（真实进程、
真实 socket）——但仍可能遇到一些粗糙之处，且配置格式在 1.0 之前可能会变化。

- 集群面向一个团队或一个人设计：几十台 worker，而不是几千台。只有一个 hub，
  没有高可用（它不在数据路径上，所以 hub 宕机只会暂停调度和注册）。
- Worker 是受信任的机器：除服务账户外没有额外的沙箱隔离。不受信任的代码请自行
  放在虚拟机或容器中运行。
- Linux 上的托盘应用需要 GTK 4 + WebKitGTK 6（或使用构建标签切换到 GTK 3）；
  macOS 安装包必须在 Mac 上签名和公证。
- 打洞无法穿透所有 NAT；两端都处于对称型 NAT 之后时，连接会保持走中继。

## 参与贡献

欢迎提交 issue 和 pull request——请参阅 [CONTRIBUTING.md](CONTRIBUTING.md)。
`go test ./...` 会在 Windows、Linux 和 macOS 上运行单元测试和端到端测试。

## 许可证

[MIT](LICENSE)。Claude Code 是 Anthropic 的产品，Copilot CLI 是 GitHub 的产品；
Longarm 是一个独立项目，与二者均无关联。
