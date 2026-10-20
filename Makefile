.PHONY: build run restart test vet

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
