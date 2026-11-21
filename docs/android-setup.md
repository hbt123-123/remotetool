# Android（Termux）运行指南

Android 端以 **Termux 运行原生二进制**的方式使用（GOOS=android、PIE、
`CGO_ENABLED=0` 纯 Go；内嵌 Web UI 的薄壳 APK 在路线图中）。
**编译冒烟 ≠ 适配完成**：Android 的后台限制、网络切换和进程生命周期
都需要按本文档配置并真机验证
（验收清单见 [platform-matrix.md](platform-matrix.md) 的 5-6 号组合）。

## 安装与启动

```bash
# 1. 安装 Termux（F-Droid 版本；Play 商店版已过时）

# 2. 下载并安装二进制（或从电脑传 dist/release/android-arm64/ 里的文件）
pkg install -y unzip
curl -LO https://github.com/hbt123-123/remotetool/releases/download/v0.1.0/remotetool-v0.1.0-android-arm64.zip
unzip remotetool-*-android-arm64.zip
mkdir -p ~/bin && cp remotetool-*/rtd remotetool-*/rt ~/bin/ && chmod +x ~/bin/rtd ~/bin/rt

# 3. 启动守护进程（显式 --home，别让它落在 app 私有目录里）
rtd --home ~/remotetool --name 我的手机
```

> 下载链接以 [Releases](https://github.com/hbt123-123/remotetool/releases)
> 页面实际版本为准（tag 名与文件名都带 `v` 前缀）。

Web UI 同样可用：手机浏览器打开 `http://127.0.0.1:27180/?t=<token>`
（token 是启动输出里那串），功能与桌面端一致。

数据目录：`~/remotetool/`（identity.key、config.json、peers.json、rtd.log）。
Android 没有 mDNS 广播 guarantee，配对时直接用电脑的 IP + 端口。

## 保持后台存活

Android 会在几分钟内冻结/杀死普通后台进程，以下三件事缺一不可：

1. **获取 wake-lock**（阻止 CPU 休眠）：
   ```bash
   termux-wake-lock
   ```
   通知栏会出现 Termux 的常驻通知（不要划掉它）。

2. **关闭电池优化**：系统设置 → 应用 → Termux → 电池 →
   「不受限制 / 无限制」。各厂商路径不同（MIUI：「省电策略 → 无限制」）。

3. **用 termux-services 常驻**（可选但推荐）：
   ```bash
   pkg install termux-services
   mkdir -p $PREFIX/var/service/rtd/log
   cat > $PREFIX/var/service/rtd/run <<'EOF'
   #!/data/data/com.termux/files/usr/bin/sh
   exec termux-wake-lock; exec $HOME/bin/rtd --home $HOME/remotetool 2>&1
   EOF
   chmod +x $PREFIX/var/service/rtd/run
   sv up rtd
   ```
   `sv up rtd` 后 rtd 由 supervisord 托管，崩溃自动拉起。

## 网络行为

- **网络切换（Wi-Fi ↔ 蜂窝）**：切换瞬间所有连接断开。rtd 的恢复路径：
  - 配了 hub：hub 链路由客户端自动重连（指数退避 1s→5min + 每 60s 注册刷新），
    之后对端可再次经中继连入——手机端无需任何操作。
  - 未配 hub：只能由对方重新发起连接（手机切回 Wi-Fi 后地址通常变化）。
    所以**手机在公网场景务必配 hub**。
- **mDNS**：Android 省电与多播限制使 mDNS 不可靠（发现时有时无）。
  局域网场景建议配对后依赖 `last_addr` 直连；发现列表为空不是故障。
- **rtd 不持有常唤醒的网络锁**：wake-lock 只保 CPU；极端省电模式下
  系统仍可能推迟网络包，表现为偶发延迟，属平台行为。

## 已知边界

- 定位为**主控端 + 文件对端**：接收文件、浏览/收发文件、当终端客户端。
  让手机长期跑转发 listener 属于未验证场景（termux 后台网络限制）。
- `os.UserHomeDir()` 在 Termux 下是 app 私有目录；默认文件授权目录
  （file_roots 缺省）会指向它——如果要让对端访问手机存储，
  在设备详情页把授权目录改到 `~/storage/shared/...`（需先
  `termux-setup-storage` 授权）。
- 内嵌 Web UI 的薄壳 APK 在路线图中（解决 Termux 安装门槛）。
