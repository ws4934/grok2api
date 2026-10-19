#!/bin/sh
# mint 容器入口：起一个共享 Xvfb :99，然后跑铸造服务（headful chromium 需要 DISPLAY）
set -e

DISPLAY_NUM="${MINT_DISPLAY_NUM:-99}"
XVFB_OPTS="-screen 0 1366x768x24 -nolisten tcp"

cleanup() {
  kill "$XVFB_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

if command -v Xvfb >/dev/null 2>&1; then
  Xvfb ":${DISPLAY_NUM}" $XVFB_OPTS &
  XVFB_PID=$!
  # 等待 X socket 就绪
  i=0
  while [ ! -S "/tmp/.X11-unix/X${DISPLAY_NUM}" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
  done
  export DISPLAY=":${DISPLAY_NUM}"
fi

exec node /mint/mint-service.mjs
