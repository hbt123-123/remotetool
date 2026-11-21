# RT 远程 (remotetool)

私密、轻量、跨 **Windows / Linux / Android** 的远程工具。三大功能：

- **远程终端** — 真实 PTY（Windows ConPTY / Unix forkpty），多会话、断线重连回放
- **文件传输** — 分块流式、断点续传、SHA-256 校验、远程文件浏览器
- **端口转发** — SSH 隧道语义（`-L` 本地转发 / `-R` 远程转发），规则持久化

每一层都是端到端加密：配对用 **6 位配对码 + SPAKE2（P-256）+ TLS 通道绑定**，
连接用 **设备证书（Ed25519）+ QUIC/TLS 1.3 + 证书钉扎**。无账号体系、无官方
服务器——局域网 mDNS 直连；跨公网用你**自建**的 hub 中继，中继只见密文。
能力可按设备收窄：终端/文件/转发开关、文件授权目录、拨号白名单。

## 快速开始

从 [Releases](https://github.com/hbt123-123/remotetool/releases) 下载对应
平台的 zip，每台设备运行守护进程：

```bash
rtd --name 我的电脑
# 控制台输出 http://127.0.0.1:27180/?t=<token>，浏览器打开即用
```

配对：A 设备 `rt pair` 生成配对码，B 设备 `rt pair A的IP:27183 <配对码>`。

完整步骤、rt 命令速查与跨公网配置见
**[docs/getting-started.md](docs/getting-started.md)**。

## 文档

### 使用者

| 文档 | 内容 |
|---|---|
| [快速开始](docs/getting-started.md) | 获取产物 → 启动 → 配对 → 三大功能速览 |
| [Windows 安装](docs/windows-installation.md) | 放置、防火墙、开机自启、ConPTY 说明 |
| [Linux 部署](docs/linux-deployment.md) | systemd 常驻、无头服务器访问 Web UI |
| [Android (Termux)](docs/android-setup.md) | 安装、后台存活三件套、已知边界 |
| [hub 部署](docs/hub-deployment.md) | VPS 上自建中继（systemd、token、FAQ） |
| [配对指南](docs/pairing-guide.md) | 配对码规则、直连/经 hub 配对、常见失败 |
| [故障排查](docs/troubleshooting.md) | 日志位置、错误信息对照表 |
| [安全说明](docs/security.md) | 威胁模型、加密与认证、按设备权限、已知缺口 |
| [常见问题](docs/faq.md) | token/端口/mDNS/卸载等高频问题 |

### 开发者

| 文档 | 内容 |
|---|---|
| [架构](docs/architecture.md) | 模块划分与运行时结构 |
| [协议设计](docs/design.md) | 配对/传输/特性协议与安全清单（改协议须同步） |
| [修复记录](docs/fix-plan.md) | v0.1 评审修复（F1–F14，已全部实施） |
| [真机验收矩阵](docs/platform-matrix.md) | 平台组合与安全验收清单 |
| [发布流程](docs/release.md) | tag → CI 测试 → 打包 → GitHub Release |
| [CHANGELOG](CHANGELOG.md) | 版本变更记录 |

## 架构一览

```
┌─────────┐  QUIC/TLS1.3 (27183/udp)  ┌─────────┐
│  设备A  │◄──────────────────────────►│  设备B  │   局域网 mDNS 直连
└────┬────┘   控制流(JSON RPC)+数据流  └─────────┘
     │        (终端/文件/转发)
     │ 跨公网：先地址解析，直连失败走中继
┌────▼────┐
│  hub    │  你的 VPS：只见密文（中继内层 TLS 端到端）
└─────────┘
```

协议细节见 [docs/design.md](docs/design.md)。

## 从源码构建

Go 1.26（与 go.mod 一致），运行时零依赖：

```bash
./build.sh          # 当前平台 -> dist/
./build.sh release  # 交叉编译 windows/linux/android -> dist/release/
./build.sh package  # release + 版本化 zip + SHA256SUMS（CI 发布用）
```

第三方库：quic-go、yamux、zeroconf、creack/pty、x/sys、x/term、x/net
（详见 go.mod）。代码遵循根目录 [AGENTS.md](AGENTS.md) 的架构边界。

## 验证

自动化（CI：ubuntu 全量含进程级 e2e + windows 单测层，均带 `-race`）覆盖
配对防爆破、断线重连、续传、权限拒绝、路径穿越、hub TTL/重连、日志无凭据
等场景；交叉编译覆盖 windows/amd64、linux/amd64、linux/arm64、android/arm64。
真机矩阵按 [docs/platform-matrix.md](docs/platform-matrix.md) 执行。

## 已知限制（v0.1）

- 老 Windows（<10 1809）无 ConPTY，终端走管道兜底（无 ANSI，推荐 `chcp 65001`）
- Android 为编译冒烟级，真机验证按 platform-matrix 进行
- UDP 打洞 P2P 直连尚未实现（直连失败即走 hub 中继）
- Android APK 薄壳、SOCKS5 出站代理在路线图中（见 [CHANGELOG](CHANGELOG.md#unreleased)）

## 许可

[MIT](LICENSE)
