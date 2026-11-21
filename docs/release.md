# 发布流程

维护者手册：如何产出一个正式版本。发布产物（多平台 zip + 校验和）由 GitHub
Actions 自动构建并挂到 GitHub Releases，本地不承担编译（见根目录 AGENTS.md）。

## 前置检查

1. `docs/platform-matrix.md` 真机验收矩阵全部勾选（含安全验收表）。
2. `CHANGELOG.md` 的 Unreleased 段整理为新的版本条目（日期 + 变更清单）。
3. CI 在 master 上全绿。

## 发布

```bash
git tag v0.1.0
git push origin v0.1.0
```

push tag 会触发 `.github/workflows/release.yml`：

1. ubuntu 上跑全量 `go test -race ./...`（任何失败即中止，不会产出版本）；
2. `./build.sh package` 交叉编译 4 个平台（windows/amd64、linux/amd64、
   linux/arm64、android/arm64）并打 zip；
3. 自动创建 GitHub Release（以 tag 命名、自动生成 notes），上传
   `remotetool-<版本>-<平台>.zip` 与 `SHA256SUMS`。

版本号来源是 `git describe --tags`，注入到 `rt` / `rtd` / `hub` 的
`-version` 输出。注意三个 main 包的 `version` 必须保持 `var`（ldflags 注入
只对 var 生效，见 AGENTS.md）。

## 发布后

1. 到 GitHub Releases 页面检查资产齐全（4 个 zip + SHA256SUMS）。
2. 下载 zip 抽查：解压后包含 rtd/rt/hub 二进制、README.md、LICENSE、
   getting-started.md；`sha256sum -c SHA256SUMS` 校验通过。
3. 在一台真机上跑 `rtd -version`，确认输出的版本号与 tag 一致
   （tag 存在时 `git describe` 会注入正确版本，而非回退值）。

## 预发布验证（可选）

正式发版前可先推一个 rc tag 验证流水线全链路：

```bash
git tag v0.1.0-rc1 && git push origin v0.1.0-rc1
# 验证通过后清理：
gh release delete v0.1.0-rc1 --yes
git push origin :refs/tags/v0.1.0-rc1
```
