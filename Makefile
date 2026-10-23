.PHONY: build run restart restart-app test vet app app-toolchain app-package app-test app-start-existing

build:
	go build -trimpath -o bin/gateway ./cmd/gateway

run:
	go run ./cmd/gateway -config config.json

# 编译 + 优雅重启：备份旧产物、SIGTERM 停机、加载 .env 启动新进程
restart:
	chmod +x scripts/restart.sh
	./scripts/restart.sh

test:
	go test -race ./...

vet:
	go vet ./...

# macOS 桌面壳（第一版）：Wails v3 + 同进程网关
app:
	./scripts/build-desktop.sh

# 下载桌面模块所需的 Go 1.26 工具链到 desktop/vendor/go（离线可构建）
app-toolchain:
	./scripts/install-go126.sh

# 编译网关 + 优雅重启 + 再以桌面壳接管现有 config.json / data（窗口 + 菜单栏）
restart-app:
	chmod +x scripts/restart-app.sh
	./scripts/restart-app.sh

# 生成可双击的 dist/Safuno.app（本地 ad-hoc 签名，未公证）
app-package:
	./scripts/package-macos.sh

app-test:
	./scripts/test-desktop.sh

# 用仓库现有 config.json/data 启动已打包的桌面 App（不会创建新的空白 profile）
app-start-existing:
	./scripts/start-desktop-existing.sh
