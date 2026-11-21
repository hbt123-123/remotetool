# Linux 部署指南

适用 amd64 / arm64 的常规发行版（Ubuntu、Debian、Alibaba Cloud Linux 等）。
部署形态：以你的用户身份常驻 rtd（systemd user 服务），守护本机终端、
文件与转发能力。

## 1. 获取并安装程序

从 [Releases](https://github.com/hbt123-123/remotetool/releases) 下载
`remotetool-<版本>-linux-<amd64|arm64>.zip`，解压后安装：

```bash
unzip remotetool-*-linux-amd64.zip
mkdir -p ~/.local/bin
cp remotetool-*-linux-amd64/{rtd,rt} ~/.local/bin/    # hub 只在 VPS 上需要
chmod +x ~/.local/bin/rtd ~/.local/bin/rt
```

校验完整性：`sha256sum -c SHA256SUMS`（与 zip 同目录时）。

确认 `~/.local/bin` 在 `PATH` 中（Ubuntu 默认包含）。

## 2. 防火墙放行

放行 QUIC 数据端口（UDP 27183）：

```bash
sudo ufw allow 27183/udp          # ufw
# firewalld: sudo firewall-cmd --permanent --add-port=27183/udp && sudo firewall-cmd --reload
```

云服务器还要在安全组放行同端口。Web UI 只监听 `127.0.0.1`，无需放行。

## 3. 先试运行一次

```bash
rtd --name 我的服务器
```

控制台打印 `http://127.0.0.1:27180/?t=<token>`；配置落在
`~/.config/remotetool/`（`RT_HOME` 或 `--home` 可覆盖）。Ctrl+C 退出后
再按下面方式常驻。

## 4. systemd 常驻（推荐：user 服务）

以你自己的用户运行，身份与数据都在该用户下：

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/rtd.service <<'EOF'
[Unit]
Description=remotetool rtd daemon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%h/.local/bin/rtd
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable --now rtd
loginctl enable-linger        # 注销后仍保持运行（服务器必备）
```

日志看 journald（rtd 同时会写 `~/.config/remotetool/rtd.log`）：

```bash
journalctl --user -u rtd -f
```

需要传给 rtd 参数时改 `ExecStart=`（如 `rtd -v` 或 `--name xxx`）。

**系统级服务**（多用户机器、服务身份隔离）改用
`/etc/systemd/system/rtd.service`，并加 `User=`、`ExecStart=... --home /var/lib/remotetool`，
目录 `chown` 给专用账户且 `chmod 700`——identity.key 等同该用户全权，
不要用 root 跑。

## 5. 无头服务器的 Web UI 访问

rtd 的 Web UI 只监听 `127.0.0.1`（有意为之，见[安全说明](security.md)）。
在无头服务器上本地没有浏览器，两种办法：

1. **SSH 端口转发**（推荐）：

   ```bash
   ssh -L 27180:127.0.0.1:27180 user@server
   # 然后在本机浏览器打开转发后地址；token 用 rt url 在服务器上获取
   ```

2. **只用 CLI**：`ssh` 进服务器直接跑 `rt status`、`rt cp`、`rt term` 等，
   不经过 Web UI。

不要通过改代码或反代把 Web UI 直接暴露到公网。

## 6. 卸载

```bash
systemctl --user disable --now rtd
rm ~/.config/systemd/user/rtd.service
rm ~/.local/bin/{rtd,rt}
rm -rf ~/.config/remotetool      # identity.key/config/peers/日志，删除即彻底移除身份
```
