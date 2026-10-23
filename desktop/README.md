# Safuno 安诺 Desktop（第一版：macOS）

桌面壳用 **Wails v3** 提供原生窗口、菜单栏和进程生命周期；业务仍然跑现有 Go 网关。Dashboard 直接加载网关自己的 `http://127.0.0.1:8080/admin/`，因此：

- 管理台 `location.origin` 正确，复制出来的 Base URL 不会变成 `wails://...`；
- 网关下发的 CSP 原样生效；
- 前端零改动，UI 版本跟随网关二进制。

网关的 CLI、Docker 和 Web 管理台部署方式保持不变；桌面壳是新增分发形态。

## 构建与运行

前置：macOS + Xcode Command Line Tools。桌面 module 使用 Go 1.26（Wails v3 要求），Go 1.23+ 的 `GOTOOLCHAIN=auto` 会自动切换。Wails v3 固定在 `v3.0.0-beta.34`，升级时先跑 `make app-test` 和一次真实窗口冒烟。

```bash
make app          # 生成 bin/safuno
make app-package  # 生成 dist/Safuno.app（本地 ad-hoc 签名）
open dist/Safuno.app
```

`open dist/Safuno.app` 使用应用自己的默认 profile（`~/Library/Application Support/Safuno/`）。如果要在当前仓库接管现有 `config.json` 和 `data/`，用：

```bash
make app-start-existing
```

它会先检查 8080 是否被占用，再以 `-config ./config.json -data-dir .` 启动，并写 `logs/app.pid` / `data/gateway.pid`。

开发时直接运行：

```bash
./bin/safuno -data-dir /tmp/safuno-dev
```

第一次启动会在数据目录生成 `config.json`、空账号池、随机 Key，并启动菜单栏 App。默认数据目录：

```text
~/Library/Application Support/Safuno/
  config.json
  gateway.db
  session-files/
  accounts.enc
  ...
  logs/gateway.log
```

Key 优先存 macOS Keychain（service: `Safuno`）；Keychain 不可用时回退到数据目录下 `0600` 的 `secrets.json`。测试时可设 `SAFUNO_DISABLE_KEYCHAIN=1` 强制走文件。

## 使用现有数据

先停掉正在占用 8080 的 CLI 网关，然后用现有 config 启动桌面壳：

```bash
./bin/safuno -config ./config.json
```

`config.json` 同目录的 `.env` 会被自动加载（仅设置尚未存在的环境变量），因此现有账号的 `*_env` 凭证可以继续使用。数据路径会按 config 文件所在目录解析，不依赖当前工作目录。

## 菜单栏

- 打开管理台
- 复制 Base URL
- 复制管理密钥
- 打开数据目录 / 日志
- 退出 Safuno（会先优雅停止网关，等待最多 15 秒落盘）

关闭窗口只是隐藏，网关继续运行；退出 App 才会停止网关。

## 当前限制（第一版）

- 仅 macOS；Windows/Linux 壳后续使用同一 Wails 架构扩展。
- 未做 Developer ID 签名和公证，`make app-package` 只适合本机运行。
- 没有首次启动向导、端口冲突 UI、自动更新（管理台已免登录）。
- 管理台的会话正文下载、WorkBuddy OAuth 弹窗需要在 WKWebView 内实测；如果不兼容，后续加原生保存/系统浏览器桥。
- 默认端口仍是 8080；与已有 CLI 网关同时运行会启动失败。

## 结构

```text
internal/app/                  # 网关 Start/Ready/Shutdown，CLI 与桌面壳共用
cmd/gateway/                   # 原 CLI 入口，已改为调用 internal/app
desktop/
  cmd/safuno/main.go         # Wails 壳、同进程启动网关、菜单栏
  cmd/safuno/assets/         # Wails 本地资源（仅诊断页）
  internal/bootstrap/          # 数据目录、config、Keychain、首次配置
scripts/build-desktop.sh
scripts/package-macos.sh
```

## Safuno 更名兼容

新安装的应用使用 Safuno 数据目录和钥匙串服务。若尚无新的配置而原有 DayDayUp profile 已存在，桌面应用会沿用该目录，避免更名后生成空账号池或更换凭证；新钥匙串不存在的凭证会回退读取旧服务，再保存到 Safuno 服务。

启动变量使用 SAFUNO_DATA_DIR、SAFUNO_CONFIG 和 SAFUNO_DISABLE_KEYCHAIN，同时兼容旧 DAYDAYUP_ 前缀；已设置的新变量优先，命令行参数仍具有最高优先级。

代码目录已由 daydayup 更名为 safuno。已有 Docker Compose 部署如需继续使用旧数据卷，应显式指定原项目名或数据卷，确认迁移后再切换为新的项目名。
