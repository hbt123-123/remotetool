# Windows 安装指南

适用 Windows 10 1809+（x64）。老版本 Windows 的终端兼容性见文末
「已知限制」。

## 1. 获取并放置程序

从 [Releases](https://github.com/hbt123-123/remotetool/releases) 下载
`remotetool-<版本>-windows-amd64.zip`，解压到任意目录，例如：

```
C:\remotetool\
├── rtd.exe     守护进程（含 Web UI）
├── rt.exe      CLI
├── hub.exe     自建中继（仅 VPS 需要，本机可不管）
├── README.md
├── LICENSE
└── getting-started.md
```

校验完整性：下载 `SHA256SUMS` 放到同目录，在 Git Bash / WSL 中执行
`sha256sum -c SHA256SUMS`；或在 PowerShell 中用
`Get-FileHash remotetool-*.zip -Algorithm SHA256` 的输出与文件内容逐一对照。

无需安装器、无需管理员权限即可运行；`hub.exe` 只在自建中继时用到。

## 2. 防火墙放行

对端要能连进来，需要放行 QUIC 数据端口（UDP 27183）。以管理员身份运行：

```powershell
netsh advfirewall firewall add rule name="remotetool rtd (QUIC)" dir=in action=allow protocol=UDP localport=27183
```

Web UI 只监听 `127.0.0.1`，不需要任何防火墙规则；局域网发现（mDNS）
在多数 Windows 防火墙下默认可用，被拦截时不影响「按 IP 配对」。

## 3. 启动方式

### 方式 A：前台运行（试运行推荐）

```
C:\remotetool\rtd.exe --name 我的电脑
```

控制台会打印 Web UI 地址（`http://127.0.0.1:27180/?t=<token>`），浏览器
打开即用。首次运行会在 `%APPDATA%\remotetool\` 下创建配置目录：

```
%APPDATA%\remotetool\
├── identity.key   设备身份密钥（Ed25519 seed，0600）——删除即重置设备身份
├── config.json    本机配置（设备名、端口、hub、转发规则）
├── peers.json     已配对设备
└── rtd.log        运行日志（追加，无轮转，过大可手动删除）
```

### 方式 B：登录自启（计划任务）

```powershell
schtasks /Create /TN "remotetool rtd" /SC ONLOGON /TR "C:\remotetool\rtd.exe"
```

- 登录时任务会弹出一个控制台窗口，打印的 token URL 从那里看；窗口被关
  也不会终止进程，token 随时可用 `rt url` 重取。
- 想完全不显示窗口：在「任务计划程序」GUI 中打开该任务，勾选
  「不管用户是否登录都要运行」。此模式下 rtd 在后台会话中运行，终端
  会话照常工作，但拿不到控制台输出——登录后用 `rt url` 获取 Web UI 地址。
- 删除自启：`schtasks /Delete /TN "remotetool rtd" /F`。

### 方式 C：多实例（隔离测试）

用 `--home` 或 `RT_HOME` 指向独立目录、换端口即可并行跑多份：

```
rtd.exe --home D:\rt-test\a --listen-port 27184 --web-port 27181
```

## 4. 开机验证

```powershell
rt.exe status    # rt 会自动读本机 rtd 的配置（同 %APPDATA%\remotetool）
```

若提示 `daemon unreachable (is rtd running?)`，说明 rtd 没在运行。

## 已知限制

- **Windows 10 1809 以下无 ConPTY**：终端降级为管道模式（无 ANSI 颜色、
  交互式程序支持差）；建议先执行 `chcp 65001` 改用 UTF-8 代码页。
- rtd 以当前用户身份运行，权限边界是「本机该用户」——请勿把 Web UI 端口
  暴露到外网（它本来就只监听 127.0.0.1）。安全模型见
  [安全说明](security.md)。
