# clip-sync

A native Go bridge between the Wayland and X11 text clipboards on Linux.
It connects to both display servers and keeps one connection to each.
It does not invoke or replace `wl-copy`, `wl-paste`, or `xclip`.

A SPICE desktop agent can use the X11 clipboard to exchange text with a remote
console. `clip-sync` connects that clipboard to native Wayland applications.
It does not replace the SPICE agent or Xwayland.

## Status

Experimental. The headless integration suite uses separate Sway and Xvfb servers.
The niri, SPICE, and remote-console path still needs end-to-end verification.

## Requirements

- Linux and one Wayland seat.
- A compositor with `ext-data-control-v1` or `wlr-data-control-unstable-v1`.
- An X11 server with XFixes and XC-MISC.
- Go 1.27 or newer to compile. The compiled binary needs no clipboard helper tools.

## Build and run

Run development commands in a Namespace Linux instance:

```sh
make build
```

Run the resulting binary inside the target desktop session:

```sh
./bin/clip-sync
```

Keep `DISPLAY`, `WAYLAND_DISPLAY`, and `XDG_RUNTIME_DIR` from that session.
`XAUTHORITY` controls X11 authentication when the session requires it.
Run only one clipboard bridge for the display pair.

The process runs in the foreground. Send SIGTERM or press Ctrl-C to stop it.
It exits on a display disconnect or protocol error. Restart it after the desktop
recovers. Clipboard data owned by the bridge may disappear when it exits.

## Behavior and limits

- Synchronizes text copied after startup. It leaves existing clipboards alone.
- Preserves UTF-8, line endings, and empty text. Converts X11 Latin-1 `STRING`
  selections to UTF-8.
- Identifies its own X11 window and Wayland offers to avoid feedback loops.
  Content comparison also suppresses copies echoed by clipboard managers.
- Discards completed reads after a newer observed clipboard change. X11 claims
  use server timestamps, and Wayland writes wait for earlier compositor events.
- Supports large X11 transfers through `INCR`. Clipboard data has a 16 MiB limit.
  Wayland pipes and X11 `INCR` transfers have 10-second deadlines. Each backend
  allows up to eight outgoing transfers at once. Regular-file paste destinations
  do not support write deadlines.
- Leaves images, file lists, rich text, and the PRIMARY selection unchanged.

Ownership loss, read failures, and unsupported formats do not clear the other
clipboard. Explicit clipboard clears do not propagate either. The protocols do
not provide a common clock, so simultaneous copies follow observed event order.

## Privacy

The bridge holds text in memory. It does not store clipboard history or log
clipboard contents.

Copied passwords also cross the bridge. X11 clients and any connected SPICE
clipboard endpoint can receive the text. Clearing one clipboard does not erase
its copy on the other side.

## Development

Run these commands in Namespace, not on the local workstation:

```sh
make check
make integration
```

`make check` checks formatting, runs `go vet`, and runs the unit suite with the
race detector. It requires a C compiler for the race detector.

`make integration` requires Sway, Xvfb, `wl-clipboard`, and `xclip`. Run it as an
unprivileged user in a disposable Linux environment. The script creates separate
display servers, disables Sway's Xwayland bridge, and stops those servers on exit.

To add a six-minute idle descriptor check after the clipboard exercises:

```sh
CLIP_SYNC_BINARY="$PWD/bin/clip-sync" CLIP_SYNC_SOAK=6m \
  sh scripts/integration.sh ./bin/integration.test \
  -test.run TestDesktopBridge -test.v -test.timeout 8m
```

## License

[MIT](LICENSE).
