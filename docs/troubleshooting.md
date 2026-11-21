# 故障排查

先分清三类信息来源，再对照错误表定位。

## 信息来源

### 1. rtd.log（权威日志）

守护进程的持久日志，与 stderr 内容一致，追加写入、**无轮转**（过大可
在 rtd 停止时手动删除，会自动重建）。位置：

| 平台 | 路径 |
|---|---|
| Windows | `%APPDATA%\remotetool\rtd.log` |
| Linux | `~/.config/remotetool/rtd.log` |
| 自定义 | `--home` / `RT_HOME` 指定目录下的 `rtd.log` |

默认 Info 级；`rtd -v` 打开 Debug（连接细节、转发拨号等）。**日志不含有
任何凭据**（Web token 只打印到控制台，不落盘）。

### 2. 活动日志（UI 里的"最近操作"）

Web UI 首页显示的操作流水（连接建立/断开、传输完成/失败、配对成功等）。
它是**内存环形缓冲（200 条）且重启即清**，只适合看最近发生了什么；
要留证据请看 rtd.log。

### 3. hub 日志

hub **不写日志文件**，只输出到 stderr。前台运行直接看终端；systemd 部署
用 `journalctl -u rtd-hub -f`。`hub --v` 打开 Debug。

## 忘记 Web UI token

token 只在启动时打印到控制台（不落日志）。随时重取：

```bash
rt url     # 输出 http://127.0.0.1:27180/?t=<token>
```

Web UI 密码框输错会提示「令牌错误」，重开 `rt url` 给的地址即可。

## 错误信息对照表

Web UI toast、`rt` 命令报错与下表字符串对照（错误串可能带上下文变量）：

| 错误信息 | 含义 | 处理 |
|---|---|---|
| `daemon unreachable (is rtd running?)` | `rt` 连不上本机 rtd | rtd 没启动/端口被改过；先起 rtd |
| `unauthorized` / 「令牌错误」 | token 不对 | 用 `rt url` 给的最新地址打开 |
| `pairing failed` | 配对失败的统一话术（码错/过期/作废/退避中） | 真实原因只在本机 rtd.log；流程见[配对指南](pairing-guide.md#常见失败) |
| `no paired device matches "..."` | 命令里的设备名/id 没匹配到 | `rt status` 确认已配对设备的准确名称 |
| `no such ...`（session/peer/path 等） | 目标不存在**或被权限策略拒绝**（两者统一话术，防探测） | 确认会话还在（设备详情页终端标签）；确认对方设备详情页「权限」没关掉对应功能 |
| `path outside allowed roots` | 文件路径不在授权目录内 | 对方设备详情页「权限 → 文件授权目录」添加该目录；Android 默认主目录在 app 私有区，见 [android-setup](android-setup.md#已知边界) |
| `too many fwd rules for this device (max 32)` | 单设备转发规则上限 | 删掉不用的规则 |
| `cross-origin write refused` | 从非本机来源向 Web UI 提交写请求 | 不要通过反代/改 hosts 从其他地址访问 UI；回到 `127.0.0.1:27180` |
| `unknown transfer id` | 取消一个已结束/不存在的传输 | 无需处理 |
| `session required` | 终端操作没有会话上下文 | 重开终端标签 |
| 传输显示 `interrupted` / 「已中断（重试可续传）」 | 网络断开，已收部分保留为 `.rtpart` | 重试同一传输自动续传 |
| 终端标签「设备离线」 | 对端连接断开 | 网络恢复后自动重连；也可手动重连 |

## 连接类问题

### 直连失败（走中继或连不上）

1. 对端防火墙/安全组是否放行 **UDP 27183**（[Windows](windows-installation.md#2-防火墙放行) /
   [Linux](linux-deployment.md#2-防火墙放行)）；
2. 云服务器安全组经常是漏配点；
3. 双方 NAT 都是对称型时直连大概率失败——**经 hub 中继是预期行为**，
   速度取决于 VPS 带宽；
4. 地址过时：设备换了网络后旧地址失效，配了 hub 会自动解析新地址，
   没配 hub 需要对方主动连入或重新按 IP 连接。

### hub 相关

- 连不上 hub：UDP **27190** 放行、token 一致、hub 进程活着（journald 看 stderr）；
- 设备端确认已注册：rtd.log 出现 `hub connected`；
- hub 重启后设备会自动重连，已建立的直连不受影响。

### Android

后台被杀、网络切换、mDNS 时有时无等现象属平台行为，专项处理见
[android-setup.md](android-setup.md)。先检查三件套：wake-lock、电池
白名单、termux-services。

## 终端字符乱码（Windows）

无 ConPTY 的老系统走管道模式：先 `chcp 65001` 切 UTF-8 再操作；
乱码多为代码页问题。

## 仍然无解时

1. `rtd -v` 重启复现，取 rtd.log 对应时间段日志；
2. 顺手带上 `rt version` / `rtd -version` 输出与平台信息。
