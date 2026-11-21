# RT 远程 设计文档

> 参考基础：`D:\project\uu` 内 UU远程（GameViewer / `com.netease.uuremote`）四平台逆向报告。
> 本文标注了哪些设计借鉴自 UU、哪些为其轻量化/私密化替代。

## 1. 目标与边界

| 维度 | 决策 |
|---|---|
| 平台 | Windows 10+、Linux (Ubuntu)、Android（Termux 二进制 / 后续 APK） |
| 功能 | 远程终端、文件传输、端口转发（「端口调用」） |
| 不做 | 屏幕串流/远程控制（UU 的重头，需 WebRTC/硬编，超出本项目范围） |
| 隐私 | 无账号、无第三方云；E2E 加密；自建 hub 只见密文 |
| 形态 | 单静态二进制守护进程 `rtd`（内置 Web UI）+ CLI `rt` + 可选 `hub` |

## 2. 身份与配对

- **设备身份**：Ed25519 长期密钥对；设备ID = SHA-256(公钥) 前 8 字节 hex（16 位）。
- **证书**：自签名 X.509（Ed25519），QUIC 握手双向出示；`ClientAuth: RequireAnyClientCert`，
  CA 校验完全让位于**证书钉扎**（对端公钥必须等于已配对记录）。
- **配对协议**（借鉴 UU 的 8 位协助码模式，去中心化）：
  1. B 生成 6 位配对码（5 分钟、单次有效；连续错码 5 次即作废邀请，并按来源
     指数退避；未配对连接另有并发信号量 + 令牌桶配额），A 在有效期内输入；
  2. 双方在 TLS 信道内运行 **SPAKE2-P256**（密码=配对码），密钥确认双向 HMAC；
  3. **通道绑定**：SPAKE2 transcript 混入证书对哈希 `transport.CertBinding`——
     以 `"rtd/cb/v1"` 为域分隔前缀，对双方叶证书 DER 按字典序排序后拼接，
     再做 SHA-256（对称，与 TLS exporter 无关）。直连时两端一致；MITM 终结
     两条信道时对端证书不同 → 确认失败。这同时防住了 PAKE 的透明转发攻击；
  4. 成功后双方落盘 `peers.json`（ID/名称/公钥/最近地址/平台/版本/能力
     —— 双方在 pair.request/pair.confirm 里交换各自的设备元数据）。
- 实现位置：`internal/identity/spake2.go`（M/N 常量 try-and-increment 派生，带
  错码/MITM/非法点单测）、`internal/daemon/pair.go`。

## 3. 传输层

- **QUIC**（quic-go v0.65）：单 UDP 承载全部功能；TLS1.3 内建；流级多路复用、
  无队头阻塞；keepalive 15s / idle 90s。
- 传输抽象 `transport.Conn / Stream`（`internal/transport`）：Open/Accept 流 +
  RemoteID/RemotePub/ChannelBinding。当前 QUIC 实现；中继场景由
  **TLS + yamux** 组合出同接口的 `muxConn`（见 §6）。

### 3.1 信道划分（借鉴 UU 的四通道模型，WebRTC DataChannel → QUIC 流）

- **控制流**（每连接 1 条，拨号方打开）：u32 前缀 JSON 帧多路复用请求/响应/
  通知（`internal/proto/rpc.go`）。方法：`hello`（交换 ID/名称/平台/版本/能力，
  后三个可选字段，老版本对端缺省为空）、`pair.*`、`fs.*`、`xfer.*`（含 `xfer.cancel`）、
  `term.open/resize/close/list`、`fwd.listen/unlisten/grant`；hub 信道另有
  `reg`/`refresh`/`resolve`/`relay`。
- **数据流**（按需多条）：首字节信道类型 + u32 前缀 JSON 头，其后为纯二进制：
  - `ChanTerm`：会话 id 定位，之后为 PTY 原始字节（输入输出同流）；
  - `ChanFile`：传输 id 定位，从续传偏移起的文件字节；
  - `ChanFwd`：仅 `RuleID` 定位——拨号目标不上网线（旧版头部携带 `Dial`
    目标的行为已被移除并拒绝），接收侧校验来源后自行解析目标：mode L 走
    规则所有者经 `fwd.grant` 登记的授权（随连接断开失效），mode R 用本机
    自己的配置规则；之后为转发 TCP 连接的双向字节。

借鉴 UU 的 16 字节 `TERM` 帧头思路（type+sessionId+len），改为「头 JSON +
裸字节」，QUIC 流本身已是消息边界可靠的字节流，无需分块帧。

## 4. 远程终端（`internal/term`）

- 服务端会话表：`term.open` 启动 PTY 返回会话 id；数据流挂接双向泵。
- **PTY 后端**：
  - Windows：ConPTY（`CreatePseudoConsole` 系，1809+）；老系统（如 Server
    2016）自动降级为管道模式（`-NoLogo`/`chcp 65001` 缓解，无 ANSI）；
  - Unix：`creack/pty` fork/exec，`$SHELL`，TERM=xterm-256color。
- **会话存活 + 回放**（借鉴 UU 的 tmux 思路但内建）：客户端断开不杀会话；
  服务端持续把输出写入 256KB 环形缓冲，重连/刷新页面时重新挂接 `ChanTerm`
  数据流即先回放缓冲再衔接新输出——不存在 `term.attach` RPC，重挂就是数据
  流本身（客户端封装为 `ClientAttach`）。
- **会话 id 通告与恢复**：拨号成功后服务端以 `{"session":N}` 文本帧下发会话
  id，客户端重连时带 `?session=N` 重挂同一 PTY；挂接前 daemon 经 `term.list`
  校验会话仍在，已结束的会话返回统一错误（UI 据此标记会话结束而非循环重连）。
  `GET /api/term/list` 列出对端上本设备的活跃会话，UI 刷新后据此恢复标签页。
- **会话配额与回收**：每设备 16 个、全局 32 个会话上限（超限统一报错）；
  服务端 reaper 回收长时间（30 分钟）无人挂接的会话——`peer_down` 仍不清
  会话，reaper 是唯一的主动清理路径。
- **进程树清理**：Unix 以会话为单位 SIGKILL（creack/pty 以 `Setsid` 启动
  shell，job-control shell 给每个 job 独立进程组，杀单组不够；Linux 扫
  `/proc` 按会话 id 枚举，其他 unix 回退杀首进程组），`Wait` 后关闭 pty
  master fd，防止孙进程持有 slave 造成的 fd/goroutine 泄漏；Windows 用
  kill-on-close Job Object 树杀，`Wait/Kill/Resize` 以互斥锁保护句柄生命周期。
- **归属隔离**：会话属于 `term.open` 的调用设备；`term.resize/close/list` 与
  数据流挂接只作用于自己的会话，他人会话一律按“不存在”处理（不泄露存在性）。
- **设备权限门**：每次 RPC 与数据流挂接前按调用方查 `config.Peer.Perm`（本机
  配置，见 §8），被收权的设备得到与“会话不存在”相同的措辞——权限缺失与
  资源缺失不可区分。
- 前端 xterm.js（vendor 本地化，无 CDN）；CLI `rt term` 原始模式
  （unix SIGWINCH / windows 轮询 resize）。

## 5. 文件传输（`internal/filex`）

借鉴 UU 的 `Ask→Confirm→Block(+ACK)→Complete` 状态机，QUIC 流化后简化：

1. **ask**：`xfer.push {id, path, size, conflict}` 或 `xfer.pull {id, src, offset}`；
2. **confirm**：接收端解析冲突（rename/skip/overwrite）、若存在
   `<final>.rtpart` 则哈希其前缀得到**续传偏移**，随响应返回 `{offset, path}`；
3. **data**：发送端从偏移起裸流文件字节（发送端同步哈希前缀，保证校验和
   覆盖全文件）；
4. **complete**：`xfer.complete {id, sha256}`，接收端等齐 EOF 与校验和
   （两事件无顺序保证，用 ready 通道合并），校验通过原子改名落盘；
   不完整时保留 `.rtpart` 供续传，校验失败/取消时删除。
5. **失败清理**：发送端早期失败（本地文件打不开、数据流建立失败）会向对端
   发送 `xfer.cancel`（5s 上限）回收接收状态；接收端中断（EOF 早于 size）
   释放接收条目并保留 `.rtpart`，事件为 `interrupted` 状态（CLI 视为终态
   退出，重试自动续传）。两侧取消事件的 `Err` 统一为 `"canceled"`。
   Web UI 的“本机”面板以空 `peer` 走本地 fs 处理器（`peer == ""` 不参与
   设备解析，杜绝空前缀命中第一个配对设备的误路由）。

fs 浏览操作：`fs.list/mkdir/rename/remove/stat`。目录推送：`filepath.WalkDir`
逐文件 push。进度经事件总线（SSE）推送 Web UI。

**文件授权目录（file_roots）**：来自远端设备的每个路径（fs.\* 与 xfer.push
的落盘路径、xfer.pull 的源路径）都必须落在该设备在本机的授权目录内
（`config.Peer.FileRoots`；缺省 = 本机用户主目录，显式空 = 全拒）。
路径先绝对化/`Clean`、展开已存在部分的符号链接，再做授权根前缀匹配
（Windows 盘符大小写不敏感）；越界统一报 `path outside allowed roots`
（已配对设备可信，报错用于排错）。相对路径按本机主目录解析（保住
`rt cp f.bin 设备B:Downloads/x.zip` 的写法）。本机面板（`peer==""`）是
操作者本人，不受此限制。`fs.list` 空路径返回授权根列表（浏览器的起始视图），
不再枚举盘符。

## 6. 发现与广域网

- **局域网**：mDNS 广播/浏览 `_rtd._udp.local.`，TXT 带 `id`/`name`/`pf`(平台)
  （`internal/discover`）；发现条目 60s 未再广播即过期（Known() 只保留活跃宣告）。
- **自建 hub**（`cmd/hub` + `internal/hub` + `internal/session/hubclient.go`）：
  - 设备→hub：QUIC（ALPN `rtd-hub/1`），`reg {token, id}` 注册、
    `refresh` 幂等注册刷新、`resolve` 地址解析、`relay` 中继请求；
  - **注册 TTL**：注册 5 分钟无 refresh 即被摘除（resolve/relay 均不再应答，
    后台 janitor 关闭其连接），设备假死不再长期占表；客户端每 60s refresh
    一次，断线后由 KeepAlive 循环指数退避（1s→5min）自动重连；
    hub 与 rtd 必须随版本同步升级（老客户端不 refresh 会在 TTL 后被摘）；
  - **加密中继**：请求方开数据流写 `{token, dst}`，hub 通知目的端
    `relay.incoming {src, token}`，目的端回开流 `{token}`，hub 按 token 配对
    后纯转发两条流（早到流缓存防竞态）。两条流内部再跑一层**设备证书的
    TLS 1.3 + yamux**（`muxConn`），因此 hub 只见密文；ChannelBinding 与
    直连一致，配对/钉扎语义不变；
  - 拨号顺序：LastAddr → mDNS 候选 → hub 解析地址 → 直连全败走中继；
  - hub 证书可选指纹钉扎（配置 `hub_pubkey`；未配置时启动告警“未校验 hub 证书”）。
- **未做**：UDP 打洞（v0.2 路线图）；hub 集群/多 token。

## 7. Web UI 与 CLI

- Web UI（`internal/web/webdist`，go:embed）：原生 JS + xterm.js（vendor 化），
  深色主题。页面：设备（配对/连接/发现）、终端（多标签会话）、文件（双栏 +
  拖拽 + 传输进度）、转发、设置（设备名/hub）。鉴权：仅 127.0.0.1 + token
  （header/query/cookie）。事件流 SSE（`peer_connecting`/`peer_up`（富
  payload：name/platform/version/caps/addr/via）/`peer_down`、传输进度）。
  设备状态由服务端聚合计算（`GET /api/peers` 与 `/api/status` 的 `peers`
  均为 `device.Device` 视图），前端不自行 join；模型见 `docs/architecture.md` §2。
- CLI `rt`：全部经 daemon 本地 HTTP API（不与守护进程抢连接；
  `term` 走 WebSocket 桥）。

## 8. 安全清单

安全分三层：**设备身份**（下面前四项）→ **会话授权**（连接即已通过身份
认证，归属校验见 §4/§5）→ **操作权限**（本机按设备收窄，见后两项）。

- [x] 配对码不离开设备（SPAKE2），错码/中间人单测覆盖
- [x] 已配对连接证书钉扎，密钥更换即拒绝
- [x] 未配对设备只在存在有效邀请时进入配对路径，否则拒绝
- [x] 中继只见密文（内层 TLS）
- [x] Web UI 仅回环 + token；cookie 认证的写请求校验 Origin（CSRF）；
      SSE 认证走 HttpOnly cookie，token 不再进 URL
- [x] 转发规则本地落盘，监听默认 127.0.0.1
- [x] 设备权限开关（`Peer.Perm`）：终端/文件/转发可按设备关闭，
      拒绝话术与资源不存在一致（无存在性泄露）
- [x] 文件授权目录（`Peer.FileRoots`）：远端指定路径必须在根内
      （Clean + 符号链接展开 + 前缀匹配），默认主目录、可全拒
- [x] 转发拨号白名单（`Peer.FwdAllowHosts`）：`fwd.grant` 的目标必须
      获本机授权（主机/IP/CIDR），默认仅回环
- [x] 凭据不入日志：web token 的完整 URL 只打印到控制台，rtd.log 不落
      token；web 层测试断言日志无凭据
- [ ] hub 多租户隔离 / 速率限制（单用户自建场景暂不需要）

## 9. 验证记录（2026-11-07，本机双实例 + hub）

配对成功/错码拒绝、8MB push/pull SHA-256 一致、4MB/6MB 半成品续传一致、
-L/-R 转发 HTTP 实测、浏览器与 CLI 终端交互、直连破坏后中继路径
（连接/列目录/传输/终端）全部通过；windows/linux/android（arm64、amd64）
交叉编译产出静态/PIE 二进制。

## 10. 自动化测试（2026-11-21，阶段 4 起）

- 单元层：各包 `*_test.go`（配对防爆破/身份/归属/路径授权/白名单/权限门/
  hub TTL/web 层 401 与 CSRF）。
- 进程层（`tests/`，CI ubuntu 全量 + `-race`；windows 跑 `-short` 单测层）：
  `tests/internal/harness` 构建 rtd/hub 并以隔离配置目录、独立端口拉起
  真实进程，经鉴权 Web API 驱动：`integration/`（会话生命周期、终端回放、
  传输往返与取消、转发与白名单、设备状态）与 `e2e/`（配对防爆破、
  日志无凭据、未授权拒绝、hub 中继与重启恢复）。
- 真机矩阵与长时间运行清单：`docs/platform-matrix.md`。
