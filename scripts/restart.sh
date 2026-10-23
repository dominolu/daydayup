#!/usr/bin/env bash
# 编译并优雅重启网关。
#
# Go 编译型单进程：代码（含 embed 的前端）改动必须重新编译并重启进程。
# 本脚本把「备份旧二进制 → 编译 → SIGTERM 优雅停机（落盘）→ 启动新进程」做成一步。
#
# 启动约定与现状一致：环境变量从 .env 加载，日志追加到 data/gateway.log，
# PID 写 data/gateway.pid。重复执行安全：没有旧进程时直接启动。
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=bin/gateway
CONFIG=config.json
LOG=data/gateway.log
PIDFILE=data/gateway.pid
PORT=$(python3 -c "import json,sys;print(json.load(open('config.json')).get('listen','127.0.0.1:8080').rsplit(':',1)[1])" 2>/dev/null || echo 8080)

# ---- 1. 定位 Go 工具链（脚本环境 PATH 可能没有 go） ----
if ! command -v go >/dev/null 2>&1; then
  for c in /usr/local/go/bin/go /opt/homebrew/bin/go "$HOME/sdk/go1.23.12/bin/go" "$HOME/sdk/go1.16.1/bin/go"; do
    if [[ -x "$c" ]]; then export PATH="$(dirname "$c"):$PATH"; break; fi
  done
fi
command -v go >/dev/null 2>&1 || { echo "error: go toolchain not found"; exit 1; }

# 项目内缓存（首次构建或受限环境）：GO 默认缓存不可写时直接用仓库内的
if [[ -d "$PWD/.gocache" ]]; then export GOCACHE="$PWD/.gocache"; fi
if [[ -d "$PWD/.tools/gomodcache" ]]; then export GOMODCACHE="$PWD/.tools/gomodcache"; fi
# 默认 GOCACHE 不可写（如受限环境）时回落到项目内目录
GOCACHE_DIR=$(go env GOCACHE 2>/dev/null)
if [[ -n "$GOCACHE_DIR" ]] && ! mkdir -p "$GOCACHE_DIR" 2>/dev/null; then
  export GOCACHE="$PWD/.tools/gocache"
  mkdir -p "$GOCACHE"
  echo "GOCACHE -> $GOCACHE"
fi

# ---- 2. 编译（先保留一份旧产物以便回滚） ----
if [[ -x "$BIN" ]]; then
  cp "$BIN" "$BIN.before-$(date +%Y%m%d-%H%M%S)"
  echo "backup: $BIN.before-$(date +%Y%m%d-%H%M%S)"
fi
echo "building $BIN ..."
go build -trimpath -o "$BIN" ./cmd/gateway

# ---- 3. 优雅停掉旧进程（SIGTERM：等在途请求结束并落盘，最多 15 秒） ----
OLD_PID=""
if [[ -f "$PIDFILE" ]]; then OLD_PID=$(cat "$PIDFILE"); fi
if [[ -z "$OLD_PID" ]] || ! kill -0 "$OLD_PID" 2>/dev/null; then
  OLD_PID=$(lsof -tnP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | head -1 || true)
fi
if [[ -n "$OLD_PID" ]] && kill -0 "$OLD_PID" 2>/dev/null; then
  echo "stopping old pid $OLD_PID ..."
  kill -TERM "$OLD_PID"
  for _ in $(seq 1 20); do
    if ! kill -0 "$OLD_PID" 2>/dev/null; then break; fi
    sleep 1
  done
fi

# ---- 4. 启动新进程（沿用现有约定） ----
if [[ -f .env ]]; then
  set -a && source ./.env && set +a
fi
nohup ./"$BIN" -config "$CONFIG" >> "$LOG" 2>&1 &
echo $! > "$PIDFILE"
echo "started pid $(cat "$PIDFILE")"

# ---- 5. 健康检查（校验的是「我们自己启动的进程」：端口可能被其他守护抢先占用） ----
for _ in $(seq 1 20); do
  MINE=$(cat "$PIDFILE" 2>/dev/null || echo 0)
  LISTENER=$(lsof -tnP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | head -1 || true)
  if [[ -n "$LISTENER" && "$LISTENER" != "$MINE" ]]; then
    echo "warning: :$PORT is served by pid $LISTENER (external supervisor?); our pid $MINE exited"
    exit 1
  fi
  if [[ -n "$MINE" ]] && kill -0 "$MINE" 2>/dev/null && curl -sf --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    echo "gateway healthy on :$PORT"
    exit 0
  fi
  sleep 1
done
echo "error: gateway did not become healthy on :$PORT (see $LOG)" >&2
exit 1
