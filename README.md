# RT 远程 (remotetool)

私密、轻量、跨 **Windows / Linux (Ubuntu) / Android** 的远程工具。三大功能：

- **远程终端** — 真实 PTY（Windows ConPTY / Unix forkpty），多会话、断线重连回放
- **文件传输** — 分块流式、断点续传、SHA-256 校验、冲突策略、远程文件浏览器
- **端口转发** — SSH 隧道语义（`-L` 本地转发 / `-R` 远程转发），规则持久化

每一层都是端到端加密：配对用 **6 位配对码 + SPAKE2（P-256）+ TLS 通道绑定**，
连接用 **设备证书（Ed25519）+ QUIC/TLS 1.3 + 证书钉扎**，杜绝中间人。
无账号体系、无第三方服务器——局域网 mDNS 直连；跨公网用你**自建**的 hub，
中继只传输密文。

安全分层：配对只是身份确认，**能力可以按设备收窄**——每台配对设备可分别
关闭终端/文件/转发，文件访问限定在授权目录内（默认主目录），转发拨号目标
默认仅允许回环（白名单可放行内网段）。均在 Web UI 设备详情页「权限」页配置。

## 快速开始

### 1. 构建

```bash
./build.sh          # 当前平台 -> dist/
./build.sh release  # 交叉编译 windows/linux/android -> dist/release/
```

产物：`rtd`（守护进程，含 Web UI）、`rt`（CLI）、`hub`（可选自建中继）。

### 2. 启动守护进程

```bash
rtd                        # 默认配置目录（Windows %APPDATA%/remotetool，Linux ~/.config/remotetool）
rtd --name 我的电脑        # 设备名
rtd --listen-port 27183 --web-port 27180   # 自定义端口
```

启动后打印 Web UI 地址：`http://127.0.0.1:27180/?t=<token>`。浏览器打开即用。

### 3. 配对（每对设备做一次）

- **A 设备**（被信任的一方）：Web UI「设备」页 → 生成配对码；或 `rt pair`
- **B 设备**：输入 A 的地址 + 配对码；或 `rt pair 192.168.1.20:27183 123456`

配对码 5 分钟有效、单次使用；SPAKE2 保证码本身不离开设备。

### 4. 使用

```bash
rt status                  # 本机与设备列表
rt ls                      # 同上
rt term 设备B              # 远程终端（原始 TTY）
rt cp ./big.zip 设备B:Downloads/big.zip    # 上传
rt cp 设备B:C:/data/report.docx ./        # 下载
rt fwd add 设备B -L 13389:localhost:3389  # 经设备B访问其远程桌面
rt fwd add 设备B -R 9090:localhost:8080   # 让设备B访问本机服务
rt fwd list / rt fwd rm <id>
rt url                     # 打印 Web UI 地址
```

Web UI 提供等价的图形界面：设备管理、双栏文件浏览器（拖拽上传）、
xterm.js 终端、转发规则管理、hub 配置。

### 5. 跨公网：自建 hub（可选）

设备不在同一局域网时，在一台 VPS 上运行 hub：

```bash
hub --listen :27190 --token 换个长随机串
```

各设备 Web UI「设置」页填入 `VPS地址:27190` 与 token（或 `rt hub vps:27190 TOKEN`）。
之后连接优先直连（含 hub 提供的地址），失败自动走 hub 中继——
**中继流内部还有一层设备证书的 TLS 会话，hub 只见密文**。
详见 [docs/deploy-hub.md](docs/deploy-hub.md)。

## 安卓

`dist/release/android-arm64/` 内为 Termux 可用二进制（GOOS=android，PIE）：

```bash
# 在 Termux 中
./rtd --home ~/remotetool
```

安卓端定位为主控端 + 文件传输对端（浏览/收发文件、当远程终端的客户端）。
后台存活（wake-lock / termux-services / 电池白名单）、网络切换与
mDNS 限制见 [docs/android.md](docs/android.md)。
后续将提供内嵌 Web UI 的薄壳 APK。

## 架构一览

```
┌─────────┐  QUIC/TLS1.3 (27183/udp)  ┌─────────┐
│  设备A  │◄──────────────────────────►│  设备B  │   局域网 mDNS 直连
└────┬────┘   控制流(JSON RPC)+数据流  └─────────┘
     │        (终端/文件/转发)
     │ 跨公网：先信令/地址解析，直连失败走中继
┌────▼────┐
│  hub    │  你的 VPS：只见密文（内层 TLS 端到端）
└─────────┘
```

每条 QUIC 连接上：1 条控制流承载 JSON RPC（hello/配对/fs 操作/传输握手/
resize 等），数据流按类型复用（`ChanTerm`/`ChanFile`/`ChanFwd`）。
协议细节见 [docs/design.md](docs/design.md)。

## 依赖

Go 1.26；运行时零依赖。第三方库：quic-go、yamux、zeroconf、creack/pty、
x/sys、x/term、x/net（详见 go.mod）。

## 已验证

自动化（CI：ubuntu 全量含进程级 e2e + windows 单测层，均带 -race）：

- 双实例直连：配对（错误码退避/5 次作废/单次有效）、连接断开重连、终端
  会话恢复回放、文件推送/拉取/取消重试、路径穿越拒绝、转发回环规则与
  目标白名单拒绝、Web 未授权 401、`rtd.log` 无 token
- hub：注册 TTL 摘除、resolve/relay 只答新鲜设备、hub 重启后设备自动重连、
  直连地址被破坏时自动经中继恢复
- 交叉编译：windows/amd64、linux/amd64、linux/arm64、android/arm64

真机矩阵（Windows/Linux/Android 组合与 Android 后台行为）按
[docs/platform-matrix.md](docs/platform-matrix.md) 逐项执行。

## 已知限制（v0.1）

- 老 Windows（<10 1809）无 ConPTY，终端走管道兜底（无 ANSI、推荐 chcp 65001）
- Android 端为编译冒烟 + 文档指引，真机验证按 platform-matrix 进行
- UDP 打洞 P2P 直连尚未实现（当前直连失败即走 hub 中继）
- Android APK 薄壳、SOCKS5 出站代理在路线图中
