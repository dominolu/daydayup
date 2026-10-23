#!/usr/bin/env bash
# 编译网关 + 优雅重启 + 再以桌面壳接管现有 config.json / data（窗口 + 菜单栏）。
#
# 与 restart.sh 的区别：restart.sh 只拉起后台网关 bin/gateway；这里在同样的
# 编译与优雅停机之后，停掉刚落起的后台实例，改由桌面壳 dist/Safuno.app
# 接管同一个配置与数据目录（等同 make app-start-existing，但会先编译）。
#
# 桌面壳必须是本次源码构建的产物：旧的 dist/Safuno.app 内嵌的是旧网关。
# 工具链缺失时先运行 make app-toolchain（脚本会本地编译工具链）。
set -euo pipefail
cd "$(dirname "$0")/.."

APP=dist/Safuno.app
CONFIG=config.json
LOG=logs/app-launch.log

# ---- 0. 前置检查（尽量在停掉线上网关之前失败） ----
[[ -f "$CONFIG" ]] || { echo "error: $CONFIG not found" >&2; exit 1; }
command -v open >/dev/null 2>&1 || { echo "error: open(1) unavailable (macOS desktop app shells need it)" >&2; exit 1; }

# ---- 1. 构建桌面壳（内部会先校验工具链，失败则不动线上进程） ----
echo "building desktop shell ..."
if ! ./scripts/build-desktop.sh; then
  echo "error: desktop build failed (missing Go 1.26 toolchain? run: make app-toolchain)" >&2
  exit 1
fi
./scripts/package-macos.sh >/dev/null
[[ -x "$APP/Contents/MacOS/Safuno" ]] || { echo "error: $APP missing after packaging" >&2; exit 1; }

# ---- 2. 优雅重启后台网关（沿用 restart.sh 的备份/停机/健康检查） ----
./scripts/restart.sh

# ---- 3. 停掉刚落起的后台实例，把端口让给桌面壳 ----
PORT=$(python3 -c "import json;print(json.load(open('$CONFIG')).get('listen','127.0.0.1:8080').rsplit(':',1)[1])" 2>/dev/null || echo 8080)
PID=""
if [[ -f data/gateway.pid ]]; then PID=$(cat data/gateway.pid); fi
if [[ -z "$PID" ]] || ! kill -0 "$PID" 2>/dev/null; then
  PID=$(lsof -tnP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | head -1 || true)
fi
if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
  echo "stopping background gateway pid $PID ..."
  kill -TERM "$PID"
  for _ in $(seq 1 20); do
    if ! kill -0 "$PID" 2>/dev/null; then break; fi
    sleep 1
  done
fi
if lsof -tnP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "error: :$PORT still in use; the desktop app could not take over" >&2
  exit 1
fi

# ---- 4. 以桌面壳接管现有 profile ----
mkdir -p logs
# A terminal launch cannot pass flags through LaunchServices reliably; the
# shell reads these environment variables, so export them for the app.
APP_PID=""
SAFUNO_DATA_DIR="$PWD" SAFUNO_CONFIG="$PWD/$CONFIG" nohup "$APP/Contents/MacOS/Safuno" \
  -config "$PWD/$CONFIG" -data-dir "$PWD" >> "$LOG" 2>&1 &
APP_PID=$!
disown "$APP_PID" 2>/dev/null || true
echo "launch requested (pid=$APP_PID, profile=$PWD)"

for _ in $(seq 1 30); do
  if curl -sf --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    LISTENER=$(lsof -tnP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | head -1 || true)
    echo "$LISTENER" > logs/app.pid
    echo "$LISTENER" > data/gateway.pid
    echo "desktop app healthy on http://127.0.0.1:$PORT/admin/ (pid=$LISTENER, profile=$PWD)"
    exit 0
  fi
  sleep 1
done
echo "error: desktop app did not become healthy; see $LOG and logs/gateway.log" >&2
exit 1
