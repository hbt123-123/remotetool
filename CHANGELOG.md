# Changelog

本项目所有显著变更记录在此文件中。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 计划中

- Android APK 薄壳（当前经 Termux 运行原生二进制）
- SOCKS5 入口转发
- Web UI Content-Security-Policy 响应头
- NAT 直连打洞优化（当前多数场景经 hub 中继）

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
