.PHONY: build run restart test vet app app-package app-test app-start-existing

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

# 生成可双击的 dist/DayDayUp.app（本地 ad-hoc 签名，未公证）
app-package:
	./scripts/package-macos.sh

app-test:
	./scripts/test-desktop.sh

# 用仓库现有 config.json/data 启动已打包的桌面 App（不会创建新的空白 profile）
app-start-existing:
	./scripts/start-desktop-existing.sh
