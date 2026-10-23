#!/usr/bin/env bash
# Launch the built desktop shell with this repository's existing config.json
# and data/ directory. Use this instead of `open dist/DayDayUp.app` when you
# want the app to take over the developer profile rather than create a fresh
# ~/Library/Application Support/DayDayUp profile.
#
# The app binary is launched directly with -config/-data-dir. LaunchServices
# (`open --env`/`--args`) can drop those values on current macOS, which would
# silently start the default profile instead of the repository one.
set -euo pipefail
cd "$(dirname "$0")/.."

ROOT="$PWD"
APP_BUNDLE="$ROOT/dist/DayDayUp.app"
APP="$APP_BUNDLE/Contents/MacOS/DayDayUp"
CONFIG="$ROOT/config.json"
PORT=$(python3 -c "import json;print(json.load(open('config.json')).get('listen','127.0.0.1:8080').rsplit(':',1)[1])" 2>/dev/null || echo 8080)

if [[ ! -x "$APP" ]]; then
  echo "error: $APP not found; run make app-package first" >&2
  exit 1
fi
if [[ ! -f "$CONFIG" ]]; then
  echo "error: $CONFIG not found" >&2
  exit 1
fi
if lsof -tnP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "error: port $PORT is already in use; stop the current gateway/app first" >&2
  lsof -nP -iTCP:"$PORT" -sTCP:LISTEN | head -5 >&2 || true
  exit 1
fi

mkdir -p "$ROOT/logs" "$ROOT/data"
nohup "$APP" -config "$CONFIG" -data-dir "$ROOT" >> "$ROOT/logs/app-launch.log" 2>&1 &
APP_PID=$!
disown "$APP_PID" 2>/dev/null || true
echo "launch requested (pid=$APP_PID, profile=$ROOT)"

for _ in $(seq 1 30); do
  if curl -sf --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    PID=$(lsof -tnP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | head -1 || true)
    [[ -n "$PID" ]] || PID="$APP_PID"
    echo "$PID" > "$ROOT/logs/app.pid"
    echo "$PID" > "$ROOT/data/gateway.pid"
    echo "gateway healthy on http://127.0.0.1:$PORT/admin/ (pid=$PID, profile=$ROOT)"
    exit 0
  fi
  sleep 1
done
echo "warning: gateway did not become healthy; see logs/gateway.log" >&2
exit 1
