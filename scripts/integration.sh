#!/bin/sh
set -eu

if [ "$#" -eq 0 ]; then
    echo "Usage: scripts/integration.sh /path/to/integration.test [arguments]" >&2
    exit 2
fi

if [ "$(id -u)" -eq 0 ]; then
    echo "Run this script as an unprivileged user in a disposable Linux environment." >&2
    exit 1
fi

runtime=$(mktemp -d)
sway_pid=
xvfb_pid=
cleanup() {
    for pid in "$sway_pid" "$xvfb_pid"; do
        if [ -n "$pid" ]; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    rm -rf "$runtime"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

export XDG_RUNTIME_DIR="$runtime"
export WLR_BACKENDS=headless WLR_RENDERER=pixman WLR_LIBINPUT_NO_DEVICES=1
unset WAYLAND_DISPLAY
printf 'xwayland disable\n' > "$runtime/sway.conf"

Xvfb -displayfd 3 -nolisten tcp -screen 0 1024x768x24 3>"$runtime/display" >"$runtime/xvfb.log" 2>&1 &
xvfb_pid=$!
sway -c "$runtime/sway.conf" >"$runtime/sway.log" 2>&1 &
sway_pid=$!

attempt=0
while [ "$attempt" -lt 100 ]; do
    for socket in "$runtime"/wayland-*; do
        if [ -S "$socket" ]; then
            WAYLAND_DISPLAY=${socket##*/}
            export WAYLAND_DISPLAY
        fi
    done
    if [ -n "${WAYLAND_DISPLAY:-}" ] && [ -s "$runtime/display" ]; then
        break
    fi
    if ! kill -0 "$sway_pid" 2>/dev/null || ! kill -0 "$xvfb_pid" 2>/dev/null; then
        echo "Headless desktop startup failed." >&2
        tail -n 30 "$runtime/sway.log" "$runtime/xvfb.log" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done

if [ -z "${WAYLAND_DISPLAY:-}" ] || [ ! -s "$runtime/display" ]; then
    echo "Headless desktop startup timed out." >&2
    exit 1
fi
read -r display < "$runtime/display"
export DISPLAY=":$display"
"$@"
