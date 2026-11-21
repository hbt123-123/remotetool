# 快速开始

本指南带你在两台设备之间完成第一次配对，并快速过一遍三大功能。平台细节
见对应安装文档：[Windows](windows-installation.md) ·
[Linux](linux-deployment.md) · [Android (Termux)](android-setup.md)。

## 1. 获取程序

从 [Releases](https://github.com/hbt123-123/remotetool/releases) 下载对应
平台的 zip（如 `remotetool-v0.1.0-windows-amd64.zip`），解压得到三个二进制：

| 程序 | 用途 |
|---|---|
| `rtd` | 守护进程。每台设备常驻一个，内嵌 Web UI |
| `rt`  | CLI。只与本机 rtd 通信，对端连接由 rtd 管理 |
| `hub` | 可选。自建中继，只在 VPS 上需要 |

校验完整性：下载 `SHA256SUMS` 与 zip 放在同一目录，执行
`sha256sum -c SHA256SUMS`。

自行编译：仓库根目录 `./build.sh`（当前平台）或 `./build.sh release`
（全部平台交叉编译），需要 Go 1.26。

## 2. 启动守护进程

在**每台**要参与互连的设备上启动 rtd：

```bash
rtd                                          # 默认配置目录
rtd --name 我的电脑                          # 设置设备显示名
rtd --listen-port 27183 --web-port 27180     # 自定义端口（默认即此）
rtd -v                                       # 调试日志
rtd -version                                 # 查看版本
```

- 配置目录：Windows 为 `%APPDATA%\remotetool`，Linux 为
  `~/.config/remotetool`；可用 `--home` 或环境变量 `RT_HOME` 覆盖。
  首次启动自动创建，内含 `identity.key`（设备身份）、`config.json`、
  `peers.json`、`rtd.log`。
- 启动后控制台打印 Web UI 地址（带一次性 token）：
  `http://127.0.0.1:27180/?t=<token>`。

端口说明：QUIC 数据端口 `27183/udp`（对端互连），Web UI 只监听
`127.0.0.1:27180/tcp`（仅本机访问）。防火墙只需放行 UDP 27183。

## 3. 打开 Web UI

浏览器打开启动时打印的地址即可，token 校验后会种 cookie，之后无需再带。
token 丢失（如关掉了控制台）随时用 `rt url` 重新获取。

若以服务方式运行（开机自启）拿不到控制台输出，登录后执行：

```bash
rt url    # 打印 http://127.0.0.1:27180/?t=<token>
```

## 4. 配对（每对设备做一次）

配对是身份确认：通过后两台设备互相认识，之后连接无需再输码。

- **A 设备**（等待被连接的一方）：Web UI 首页「生成配对码」；或 CLI：

  ```bash
  rt pair
  # pairing code: 482913  (valid until 14:05)
  ```

- **B 设备**：Web UI 首页「连接其他设备」输入 A 的地址 + 配对码；或 CLI：

  ```bash
  rt pair 192.168.1.20:27183 482913
  ```

配对码 6 位数字、5 分钟有效、单次使用；SPAKE2 保证码本身不离开设备。
局域网内 A 的地址通常是「IP:27183」（可在 A 的 Web UI 首页发现列表或
`rt status` 查看）。跨公网配对请先配置 hub（见下节），然后同样用
「地址+配对码」完成。语义与失败行为详见 [配对指南](pairing-guide.md)。

## 5. 使用三大功能

Web UI（首页点进设备详情页）与 CLI 等价：

| 功能 | Web UI | CLI |
|---|---|---|
| 远程终端 | 设备详情页 → 终端（多标签、断线重连回放） | `rt term 设备B` |
| 文件传输 | 设备详情页 → 文件（双栏浏览器） | `rt cp ./big.zip 设备B:Downloads/big.zip`（上传）、`rt cp 设备B:C:/data/report.docx ./`（下载） |
| 端口转发 | 设备详情页 → 转发 | `rt fwd add 设备B -L 13389:localhost:3389`（本机经设备B访问其远程桌面）、`rt fwd add 设备B -R 9090:localhost:8080`、`rt fwd list` / `rt fwd rm <id>` |

常用查询命令：

```bash
rt status        # 本机与已配对/在线设备
rt url           # Web UI 地址
rt hub           # 查看当前 hub 配置
rt version
```

## 6. 跨公网：自建 hub（可选）

设备不在同一局域网时，在一台 VPS 上运行自建中继：

```bash
hub --listen :27190 --token 换个长随机串
```

各设备配置（Web UI「设置」页，或 CLI）：`rt hub 你的VPS:27190 TOKEN`。
之后连接优先尝试直连，失败自动经 hub 中继——**中继流内部还有一层端到端
TLS，hub 只见密文**。部署细节见 [hub 部署指南](hub-deployment.md)。

## 下一步

- 收紧权限：新配对设备默认全功能开放，建议在设备详情页「权限」里按需
  关闭终端/文件/转发、限定文件目录与转发目标——见[安全说明](security.md)。
- 遇到问题：先看[故障排查](troubleshooting.md)与[常见问题](faq.md)。
