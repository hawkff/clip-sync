# clip-sync

clip-sync keeps the Wayland and X11 clipboards in sync on Linux.
Copy in a Wayland app and paste into an X11 app, or the other way around.
Text, images, rich text, and file lists carry over, and so does the middle-click selection.

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
Content that clip-sync copied to the other side can disappear when it exits.

## What syncs

clip-sync syncs what you copy after it starts.
It leaves both clipboards alone at startup.

The clipboard and the middle-click PRIMARY selection sync separately.
PRIMARY needs a compositor with primary selection support.

clip-sync copies these formats when an app offers them, and leaves other formats on their own side:

- Text. UTF-8 text, line endings, and empty text arrive unchanged.
  Latin-1 `STRING` text from X11 apps becomes UTF-8.
- Rich text as `text/html` and `text/rtf`.
- File lists as `text/uri-list`, plus the GNOME and KDE types that mark a copy or a cut.
- One image, preferring `image/png`, then JPEG, GIF, WebP, and BMP.

## Behavior

- Each format can be up to 16 MiB, and large X11 transfers use `INCR`.
  clip-sync drops any transfer that takes longer than 10 seconds.
- Clearing a clipboard clears the other one too, and so does closing the app that owns it.
  clip-sync clears the other side only when it held the same content, so a newer copy there survives.
- If a transfer fails, or an app offers only formats clip-sync doesn't copy, the other clipboard keeps its content.
- clip-sync ignores clipboard entries it created and skips content the other side already has.
  Copies don't loop, even when a clipboard manager re-copies them.
- If you copy again while a slow transfer is running, clip-sync discards the older one.
  Wayland and X11 share no clock, so if you copy on both sides at once, the copy clip-sync sees last wins.

## Privacy

clip-sync holds clipboard content in memory.
It doesn't keep a history or log clipboard contents.

It syncs copied passwords like any other text.
Any X11 client and any connected SPICE clipboard endpoint can read them.
When a password manager clears the clipboard after a timeout, clip-sync clears the other side too.

## License

[MIT](LICENSE)
