# Changelog

本项目所有显著变更记录在此文件中。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 修复（2026-11 整体审查，详见 docs/code-review-2026-11.md）

- Web UI：终端断线重连后不再重复注册输入监听（重连后按键翻倍）；
  访问令牌不再写入 localStorage（只保留 HttpOnly cookie 认证）；
  SSE 重连后补拉活动日志
- 进程稳定性：所有 RPC handler goroutine 顶层 recover，单个 handler
  panic 不再击穿整个守护进程/hub；ConPTY 属性列表 sizing 失败不再越界
- 终端配额（每设备 16 / 全局 32）在 PTY 孵化前预留，并发打开无法绕过
- 文件传输：校验失败如实返回发送端（此前发送方误报成功）；接收条目
  加每设备/全局上限与 30 分钟空闲回收，防止句柄泄漏；权限收权对进行中
  的数据流同样生效；小文件拉取不再被误判为中断
- hub：新增总连接数上限与"30 秒未注册即踢出"（KeepAlive 不再永久保活
  僵尸连接）；resolve/relay 加每设备令牌桶；token 常量时间比较
- 会话层：hub 断开瞬间 Resolve/Relay 不再对 nil 连接解引用；hub RPC 全部
  有超时上限；入站握手并发上限（单连接不再能阻塞 accept 10 秒）；
  配对握手处理器统一在 Serve 前挂接
- 端口转发：对端断连即清其监听器（端口不再被永久占用）；grant 归属校验
  与每设备上限
- CLI：`rt fwd add <设备> -L ...`（文档原示例）可用；`rt cp` 在进度流
  意外中断时以非零码退出（不再假成功）；`rt term` WebSocket 单写者化
  （消除帧交错与 PayloadType 竞态）；设备名解析失败时报告真实原因
- Windows：ConPTY 正确引用含空格的 shell 路径（pwsh 默认安装位可用）
- 配置：config.json / peers.json 损坏时移入 `.corrupt-*` 并重新初始化
  （此前守护进程永久无法启动）；peers.json 写失败记录日志
- Web 服务：新增 CSP / nosniff / X-Frame-Options / Referrer-Policy 响应头
- 协议（随版本同步发布）：终端 resize 帧改为 `\x00{json}` 前缀（手工输入
  的 JSON 不再被误判为命令）；identity.key 容忍尾随空白

### 计划中

- Android APK 薄壳（当前经 Termux 运行原生二进制）
- SOCKS5 入口转发
- NAT 直连打洞优化（当前多数场景经 hub 中继）
- 依赖替换：停更的 zeroconf（mDNS）与已冻结的 x/net/websocket

## [0.1.0] - 2026-11-21

### 新增

- 统一设备模型与生命周期状态管理；设备中心 UI（首页 / 设备详情页 / 活动日志）
- 远程终端：会话列表与自动恢复、多标签会话、PTY 归属校验与会话树清理
- 文件传输：上传 / 下载、中断续传、按传输 ID 取消、`file_roots` 路径白名单
- 端口转发：`-L` / `-R` 规则、自定义名称、运行时状态展示、按设备拨号白名单
- 配对：SPAKE2-P256 + TLS 通道绑定的 6 位配对码；防爆破（错 5 次作废、指数退避、未配对限流）
- 自托管 hub 中继：设备注册 TTL 清理、KeepAlive 重连退避
- 按设备权限模型：终端 / 文件 / 转发功能开关，文件授权目录，拨号白名单
- mDNS 局域网发现（Android 上不可靠，支持按 IP 配对兜底）
- 进程级 e2e / 集成测试（`tests/`），CI 双平台矩阵（ubuntu 全量 `-race` + windows `-short`）

[unreleased]: https://github.com/hbt123-123/remotetool/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/hbt123-123/remotetool/releases/tag/v0.1.0
