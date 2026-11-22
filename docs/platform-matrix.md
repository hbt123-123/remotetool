# 跨平台验证矩阵

> 本文档是阶段 4（安全与跨平台稳定性）的真机验收清单。进程级自动化测试
> （`tests/integration`、`tests/e2e`）已在 CI（ubuntu）覆盖同机直连/中继路径，
> 本矩阵覆盖**真机组合**——特别是 Android 的后台存活与网络切换行为，
> 这是编译冒烟无法证明的部分。
>
> 约定：`主控` = 发起连接/开终端的一方；`对端` = 被连接的一方。
> 二进制来自 `./build.sh release` 的 `dist/release/<os>-<arch>/`。

## 环境准备（每个组合做一次）

1. 主控与对端分别安装对应架构的 `rtd`/`rt`，各自启动：
   `rtd --name <设备名>`（Android 见 [android-setup.md](android-setup.md)）。
2. 对端 Web UI 生成配对码 → 主控 `rt pair <对端IP>:27183 <码>` 完成配对。
3. 记录双方设备 ID（`rt status`）。

## 矩阵

| # | 组合 | 配对 | 终端 | 文件 | 转发 | 断线恢复 |
|---|------|------|------|------|------|----------|
| 1 | Windows → Windows | ☐ | ☐ | ☐ | ☐ | ☐ |
| 2 | Windows → Linux | ☐ | ☐ | ☐ | ☐ | ☐ |
| 3 | Linux → Windows | ☐ | ☐ | ☐ | ☐ | ☐ |
| 4 | Linux → Linux | ☐ | ☐ | ☐ | ☐ | ☐ |
| 5 | Android → Windows | ☐ | ☐ | ☐ | ☐ | ☐ |
| 6 | Android → Linux | ☐ | ☐ | ☐ | ☐ | ☐ |
| 7 | 三端经 Hub | ☐ | ☐ | ☐ | ☐ | ☐ |

### 1-4 桌面组合

各步骤在主控上执行（对端地址记为 `<peer>`）：

**终端**
```bash
rt term <peer>            # 进入后执行: echo ok、htop/tasklist（应实时刷新）
                          # Ctrl+D 退出；再开两个会话，关闭窗口重开 Web UI → 会话恢复
```
预期：输出实时、中文不乱码、断开后重进能看到回放（256KB 滚动缓冲）。

**文件**
```bash
rt cp ./10mb.bin <peer>:Downloads/10mb.bin        # 上传
rt cp <peer>:Downloads/10mb.bin ./down.bin        # 下载
# 传输中途 Ctrl+C → 立即重传同一路径 → 应从断点续传完成，SHA-256 与源一致
rt ls <peer>                                      # 远程浏览
```

**转发**
```bash
# 对端跑一个服务（如 python3 -m http.server 8080）
rt fwd add <peer> -L 18080:localhost:8080
curl http://127.0.0.1:18080/                      # 应拿到对端页面
rt fwd add <peer> -R 19090:localhost:9090         # 主控上跑 9090 服务，对端 curl 19090
rt fwd list                                       # 状态列应显示 running
```

**断线恢复**
```bash
# 杀对端 rtd → 重启 → 主控 rt ls <peer>（应自动直连恢复）
# Web UI 里对端状态: connected → offline → connected
# 转发规则应随 peer_up 自动重新生效（无需重加）
```

### 5-6 Android 主控

Android 端定位为主控端（连接管理、终端、文件）。除上述步骤外，重点验证：

- **网络切换**：Wi-Fi ↔ 蜂窝切换后 1 分钟内，主控重新点击连接应恢复
  （hub 场景由自动重连兜底，见下）；终端会话在对端仍存活，重连后回放。
- **后台回收**：主控退到后台 10 分钟再回来，Web UI 重开后设备状态应
  正确刷新；对端仍在线。
- **对端为 Android 的注意项**：mDNS 在 Android 省电模式下不可靠，配对建议
  直接用 IP；详见 [android-setup.md](android-setup.md)。

### 7 三端经 Hub

1. VPS 上 `hub --listen :27190 --token <token>`（部署见 [hub-deployment.md](hub-deployment.md)）。
2. 三台设备的 Web UI 设置页都填 hub 地址与 token。
3. 断开设备间直连的可行性（如手机关 Wi-Fi），验证 `connected` 标签为「中继」。
4. 中继路径下跑一轮：终端 echo、10MB 文件传输、-L 转发 curl。
5. **hub 重启恢复**：`kill` hub 进程再原样重启（同端口同证书目录）→
   各设备应在一分钟内自动重连 hub（无需操作），中继连接可再次建立。
   （这是 v0.1.0 起的行为：客户端带注册刷新 + 指数退避自动重连。）
6. **hub 长时间不可用**：停止 hub 30 分钟后重启 → 设备仍应自动恢复。

## 安全验收（随矩阵一起跑）

| 验收项 | 步骤 | 预期 |
|--------|------|------|
| 连续错误配对 | 对端生成配对码，主控连错 5 次 | 第 5 次后邀请作废，正确码也拒绝；期间有指数退避 |
| 身份冒充 | 复制 A 的 config 目录到另一台机器改端口连 B | 证书不匹配，拒绝 |
| 会话越权 | A 开终端，B（另一配对设备）尝试附加 | 按不存在处理 |
| 路径穿越 | `rt cp evil.bin <peer>:../escaped.bin` | 拒绝（`path outside allowed roots`） |
| 授权目录 | 对端设备详情页把 A 的授权目录改为空 | A 的所有文件操作被拒 |
| 转发白名单 | 对端把 A 的拨号白名单清空 | A 的 -L 规则立即失效且报 not allowed |
| Web 未授权 | `curl http://127.0.0.1:27180/api/status` | 401 |
| 日志无凭据 | `grep <web_token> <RT_HOME>/rtd.log` | 无匹配 |

## 结果记录

每个格子通过后打勾并注明日期与版本（`rt version`）。发现问题记入
issue/fix-plan，附组合编号与步骤号。
