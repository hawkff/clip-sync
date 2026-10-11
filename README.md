# clip-sync

clip-sync keeps the Wayland and X11 text clipboards in sync on Linux.
Copy text in a Wayland app and paste it into an X11 app, or the other way around.

It's a single Go binary with one connection to each display server.
It doesn't run `wl-copy`, `wl-paste`, or `xclip`.

One example is a virtual machine with a SPICE agent.
The agent shares the X11 clipboard with the remote console, and clip-sync connects that clipboard to your Wayland apps.
You still need the SPICE agent and an X server such as Xwayland.

clip-sync is experimental.

## Requirements

- Linux with a single Wayland seat
- A compositor that supports `ext-data-control-v1` or `wlr-data-control-unstable-v1`
- An X11 server with the XFixes and XC-MISC extensions
- Go 1.27 or newer for `go install`

## Install

```sh
go install github.com/hawkff/clip-sync@latest
```

Go puts the binary in `$(go env GOPATH)/bin`, or in `$GOBIN` if you set it.
Prebuilt amd64 and arm64 binaries are on the [releases page](https://github.com/hawkff/clip-sync/releases).

## Usage

Run clip-sync inside your desktop session:

```sh
clip-sync
```

It needs `WAYLAND_DISPLAY`, `XDG_RUNTIME_DIR`, and `DISPLAY` from that session.
It also reads `XAUTHORITY` when the X server requires authentication.
Only one clip-sync can run per Wayland and X11 display pair, and a second one exits with an error.

clip-sync runs in the foreground until you press Ctrl-C or send it SIGTERM.
It exits if either display disconnects or reports a protocol error.
Restart it after the desktop recovers.
Text that clip-sync copied to the other side can disappear when it exits.

## Behavior

clip-sync syncs text you copy after it starts.
It leaves both clipboards alone at startup.

- UTF-8 text, line endings, and empty text arrive unchanged.
  Latin-1 `STRING` text from X11 apps becomes UTF-8.
- Text can be up to 16 MiB, and large X11 transfers use `INCR`.
  clip-sync drops any transfer that takes longer than 10 seconds.
- If a transfer fails or the app that owns a clipboard exits, the other clipboard keeps its text.
- clip-sync ignores clipboard entries it created and skips text the other side already has.
  Copies don't loop, even when a clipboard manager re-copies them.
- If you copy again while a slow transfer is running, clip-sync discards the older one.
  Wayland and X11 share no clock, so if you copy on both sides at once, the copy clip-sync sees last wins.

clip-sync doesn't sync (yet):

- Images, file lists, rich-text formatting, or the middle-click PRIMARY selection.
- Clipboard clears. If you clear one clipboard, the other keeps its text.

## Privacy

clip-sync holds clipboard text in memory.
It doesn't keep a history or log clipboard contents.

It syncs copied passwords like any other text.
Any X11 client and any connected SPICE clipboard endpoint can read them.
Clearing one clipboard doesn't clear the other, so a password manager's auto-clear may leave the password on the other side.

## License

[MIT](LICENSE)
