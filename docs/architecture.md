# RT 远程 产品与架构（阶段 1–2）

> 本文是产品与架构层面的统一视图：设备模型、连接生命周期、平台职责与模块边界。
> 协议细节见 `docs/design.md`，两者改协议时须同步。

## 1. 产品中心：设备，而非功能页

用户的思考方式是「连接我的服务器，然后开终端 / 传文件 / 转发端口」。
因此产品的中心是**设备**，终端 / 文件 / 转发是设备的能力（caps），不是并列的功能页。

- 阶段 2 已把 UI 收敛为设备中心的信息架构（仍为嵌入式 vanilla JS，无构建步骤）：
  - **首页 = 设备总览**：统计条（设备数 / 在线 / 活跃会话 / 传输中，全部由前端从
    `peers[].status`、活跃终端标签与传输进度计算）、设备卡片网格（状态点、caps、
    直连/中继徽章、最近在线）、最近操作记录。
  - **设备详情页**（`#/device/<id>`，hash 路由可刷新恢复）：设备信息头 +
    终端 / 文件 / 转发三个 tab 内嵌复用功能页逻辑（文件双栏与转发列表按容器参数化，
    终端 pane 在两个页面间移动复用）。功能页保留为独立入口。
  - 所有页面消费统一的 `device.Device` 视图，状态由服务端计算，前端不再自行 join。

## 2. 设备数据模型

统一模型定义在 `internal/device`（纯模型包，零状态、零外部依赖），
由各层填充：持久态在 `config.Store`，运行态在 `session.Manager`，
发现态在 `discover.MDNS`，聚合出口在 `daemon.deviceViews()`。

| 字段 | 含义 | 来源 |
|---|---|---|
| `ID` | 16 位 hex 设备 ID = SHA-256(Ed25519 公钥) 前 8 字节 | 证书（单一可信源） |
| `Name` | 用户自定义名称（hostname 兜底） | config / 配对交换 / hello 同步 |
| `Platform` | windows / linux / android | `runtime.GOOS`，经 hello、pairing、mDNS TXT(`pf=`) 上报 |
| `Version` | 本端 remotetool 版本 | `cmd/rtd` 的 `var version`（ldflags 注入）经 Options 下传 |
| `Caps` | 功能名列表（term/file/fwd） | hello / pairing 交换并落盘 |
| `Status` | 生命周期状态（见下） | `device.Aggregate` 聚合 |
| `LastSeen` | 最近一次成功握手时间 | `config.TouchPeer`（每次会话建立刷新） |
| `LastAddr` | 最近一次成功使用的**可拨号**地址 | `TouchPeer`；中继会话不写（防污染） |
| `Conn` | 当前会话的路径信息 `{kind, addr}` | 仅 connected 时有值 |

### 2.1 生命周期状态（Status ≠ TCP 连接状态）

| 状态 | 语义 | 证据 |
|---|---|---|
| `offline` | 无任何可达性证据 | — |
| `reachable` | 存在可用网络路径，但无法证明对端当前存活 | `LastAddr` 非空，或本机 hub 链路存活 |
| `online` | 设备最近完成过服务宣告（心跳类信号） | mDNS 广播 60s 内见过（与 discover 的 entryTTL 一致） |
| `connecting` | 拨号进行中 | session.Manager 的 dialing 表 |
| `connected` | 存在活跃会话（QUIC keepalive 15s 维持活性） | session.Manager 的 conns 表 |

聚合优先级：`connected > connecting > online > reachable > offline`，实现为纯函数
`device.Aggregate(Evidence, now)`（表驱动单测覆盖）。

**已知缺口（诚实性）**：hub 场景下无法廉价得知对端是否已向 hub 注册
（resolve 有限流 10/s 且 hub 无列表 API），故 hub 链路只映射为 `reachable`。
阶段 4 已给设备↔hub 补上注册刷新（60s `refresh` + hub 5min TTL）与自动重连，
但"对端已注册"的 hub 证据仍缺——待 resolve 按需探测（阶段 2 设备间 ping 一并考虑）。

## 3. 统一连接生命周期

```
未配对 ──配对码+SPAKE2──▶ 已配对(peers.json) ──mDNS/hub/last_addr──▶ 发现
   ▲                            │                                     │
   │ 错码5次作废邀请             │                                     ▼
   │                            │              Manager.Connect（single-flight）
   │                            │                            │
   │                            │                     peer_connecting 事件
   │                            ▼                            ▼
   └──重新配对(pair.* 常驻每条控制流)─── 已配对 ◀──dialHandshake/acceptHandshake
                                                (TLS 证书钉扎 + hello 元数据交换)
                                                       │
                                              TouchPeer(LastAddr/LastSeen/meta)
                                                       ▼
                                        已连接（feature AttachRPC + 数据流）
                                                       │ rc.Done
                                              peer_down 事件 + 会话清理
```

| 阶段 | 实现位置 |
|---|---|
| 配对（邀请/SPAKE2/防爆破） | `internal/daemon/pair.go`、`internal/identity/spake2.go` |
| 发现 | `internal/discover`（mDNS，60s 过期）、hub resolve |
| 建立连接 | `internal/session/dialer.go`：LastAddr → mDNS 候选 → hub 解析 → 全败走中继 |
| 身份认证 | QUIC TLS 层证书钉扎（`dialer.pinVerify`）；通道绑定 `transport.CertBinding` 只用于配对 |
| 会话创建 | `internal/session/handshake.go`：hello 元数据交换 + `register`（glare 处理、feature 挂载） |
| 使用 | Feature（term/filex/fwd）经 `AttachRPC/HandleStream` 完全复用 session 层，无自建连接 |
| 断开 | `rc.Done()` 回调：conns 清理 + `peer_down`（term/filex 的会话/续传刻意跨连接存活；term 会话由服务端 reaper 按空闲 TTL 回收） |

对外 API：`Manager.StatusOf`（connected/connecting，空=交聚合层，deviceViews 的唯一运行态证据入口）、
SSE 事件 `peer_connecting` / `peer_up`（富 payload：name/platform/version/caps/addr/via）/ `peer_down`。

## 4. 连接路径：直连与中继是同一条路径的不同形态

- 数据面统一为 `transport.Conn` 接口：直连 = QUIC（quicConn），中继 = TLS1.3+yamux（muxConn）；
  握手 / hello / feature 挂载 / TouchPeer 全部共用同一代码，`PathKind`（direct/relay）
  仅作为标识随地址标记（`relay` / `relay:<src>`）传递。
- 中继地址**不落盘**：`TouchPeer` 收到 relay 标记地址时只刷 `LastSeen`，
  保证 `LastAddr` 永远是可拨号地址（修复了老版本把 `"relay"` 写进 peers.json、
  下次直连白试一次的缺陷）。
- 中继语义与直连一致：内层 TLS 用设备证书，`ChannelBinding` 对称公式相同，
  SPAKE2 配对与证书钉扎在两条路径上行为不变；hub 只见密文。

## 5. 三端职责

| 平台 | 角色 | 优先支持 |
|---|---|---|
| Windows | 桌面主控 + 被控（ConPTY） | 完整桌面操作：Web UI / CLI、终端、文件、转发 |
| Linux | 服务端 + 主控 | 稳定运行、命令行部署（systemd/容器）、creack/pty |
| Android | 移动主控 | 连接管理、终端、文件（Termux 二进制，APK 后续） |

**不强行统一**：平台相关的终端实现（ConPTY vs creack/pty）、文件系统访问（盘符列表等）、
进程管理保留在平台适配层；统一的是协议（`proto`）、认证（`identity`）与业务规则（session 层）。

### 5.1 cmd 命名与未来拆分路径

当前刻意维持三个二进制（决策：不过早拆分程序——不为架构漂亮增加部署复杂度）：

| 二进制 | 当前职责 | 对应目标结构概念 |
|---|---|---|
| `cmd/rt` | 薄 CLI，只与本机 rtd 的 web API 通信 | desktop 主控入口 |
| `cmd/rtd` | 守护进程：被控服务 + 主控 API + Web UI | agent（被控）+ desktop（主控）核心 |
| `cmd/hub` | 自托管中继 | hub |

拆分 `desktop` / `agent` 为独立二进制的时机：当出现「只做被控、无 UI 需求」的部署形态
（如无头 NAS agent）或 rtd 单进程的职责膨胀到需要独立发布节奏时。拆分前先完成的准备
已就位：认证/连接逻辑集中在 session 层，agent 只需 `session.Manager + Feature` 组装。

## 6. 模块边界

| 现有位置 | 职责 | 目标结构对应 | 是否迁移 |
|---|---|---|---|
| `internal/device` | 统一设备模型 + 状态聚合（纯模型） | device | ✅ 本阶段新建 |
| `internal/config` | 持久化（config.json / peers.json） | device（持久态） | 不动 |
| `internal/session` | 连接生命周期编排（握手/连接表/中继客户端） | session | 不动 |
| `internal/daemon/pair.go` | 配对流程 | pairing | 不动（配对与 daemon 组装强耦合，迁移收益低） |
| `internal/identity` | 证书/SPAKE2/通道绑定 | security | 不动（已单一职责） |
| `internal/transport` | QUIC 封装 + Conn 接口 | transport | 不动 |
| `internal/term` `filex` `fwd` | 三大 Feature | 同名 | 不动（已通过 Feature 接口解耦） |
| `internal/web` + `webdist` | webAPI + 嵌入式 UI | web | 不动 |
| `internal/hub` | hub 服务端 | hub | 不动 |

**认证与连接逻辑唯一性约束**：所有 Feature 与新增能力必须复用 `session.Manager` 的
连接与认证（Feature.AttachRPC/HandleStream），禁止自建拨号/认证路径；
所有 RPC handler 与 HandleStream 必须校验归属与设备权限，资源不存在与
权限拒绝共用统一措辞（防存在性探测）。

## 7. 已知简化与后续阶段

- **心跳（部分落地）**：设备↔hub 已有注册刷新（60s `refresh` + hub 侧 5min
  TTL，断线由 KeepAlive 指数退避重连——阶段 4）；设备↔设备间仍无应用层
  ping，活性靠 QUIC keepalive（会话内）与 mDNS 广播（局域网在线）。
- **活动日志为内存实现**：`internal/daemon/activity.go` 的 200 条环形缓冲，重启即清空
  （定位是"最近操作"信息流而非审计；持久记录由 slog 落 rtd.log）。
- **fwd 规则状态**：owner 侧内存状态表（`fwd.Service.status`）记录每条规则的
  运行中/已停止、启动时间与最近一次 apply 失败原因；连接断开时该连接的规则标记为
  已停止（`rc.Done` watcher），重连后 `peer_up` 自动重新 apply。`GET /api/fwd` 的
  `active` 字段为 CLI 兼容保留，UI 消费 `status`。规则可选 `name` 仅为本机展示
  字段，不上线缆。
- **浏览器直传未实现**：文件页拖放只给出提示；真实上传需要浏览器 File → 本机 rtd
  的二进制通道（涉及新端点的安全评审），留待后续阶段。
- **UDP 打洞**：v0.2 路线图（design.md §6）。
