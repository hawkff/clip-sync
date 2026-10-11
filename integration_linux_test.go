//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	control "github.com/AvengeMedia/dankgo/wayland/ext_data_control"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

type testProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startTestProcess(t *testing.T, cmd *exec.Cmd) *testProcess {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &testProcess{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(p.stop)
	return p
}

func (p *testProcess) stop() {
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
}

type desktop struct {
	t      *testing.T
	owners [2]*testProcess
}

func (d *desktop) copy(from side, data []byte, mime string) {
	d.t.Helper()
	if d.owners[from] != nil {
		d.owners[from].stop()
	}
	var cmd *exec.Cmd
	if from == waylandSide {
		cmd = exec.Command("wl-copy", "--foreground", "--type", mime)
	} else {
		cmd = exec.Command("xclip", "-selection", "clipboard", "-in", "-quiet", "-target", mime)
	}
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	d.owners[from] = startTestProcess(d.t, cmd)
}

func (d *desktop) await(from side, want []byte) {
	d.t.Helper()
	mime := "text"
	if from == x11Side {
		mime = "UTF8_STRING"
	}
	awaitFormat(d.t, from, selClipboard, mime, want)
}

func paste(from side, sel selection, mime string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if from == waylandSide {
		args := []string{"--no-newline", "--type", mime}
		if sel == selPrimary {
			args = append(args, "--primary")
		}
		cmd = exec.CommandContext(ctx, "wl-paste", args...)
	} else {
		cmd = exec.CommandContext(ctx, "xclip", "-selection", x11SelectionName(sel), "-out", "-target", mime)
	}
	// A Wayland source can retain the stdout descriptor after wl-paste exits.
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.Output()
}

func awaitFormat(t *testing.T, from side, sel selection, mime string, want []byte) {
	t.Helper()
	// Outlast the bridge's transfer timeout, which can skip a stalled format first.
	deadline := time.Now().Add(transferTimeout + 2*time.Second)
	var got []byte
	var err error
	for time.Now().Before(deadline) {
		got, err = paste(from, sel, mime)
		if err == nil && bytes.Equal(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("side %d selection %d %s mismatch: got %d bytes, want %d, error=%v", from, sel, mime, len(got), len(want), err)
}

func awaitEmpty(t *testing.T, from side, sel selection) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if clipboardEmpty(t, from, sel) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("side %d selection %d did not become empty", from, sel)
}

func clipboardEmpty(t *testing.T, from side, sel selection) bool {
	t.Helper()
	if from == x11Side {
		return x11Owner(t, sel) == 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	args := []string{"--list-types"}
	if sel == selPrimary {
		args = append(args, "--primary")
	}
	cmd := exec.CommandContext(ctx, "wl-paste", args...)
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	// A killed wl-paste also fails, so only a normal non-zero exit means empty.
	var exit *exec.ExitError
	return ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() > 0
}

func x11SelectionName(sel selection) string {
	if sel == selPrimary {
		return "primary"
	}
	return "clipboard"
}

func x11Atom(t *testing.T, conn *xgb.Conn, name string) xproto.Atom {
	t.Helper()
	reply, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return reply.Atom
}

func x11Owner(t *testing.T, sel selection) xproto.Window {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reply, err := xproto.GetSelectionOwner(conn, x11Atom(t, conn, strings.ToUpper(x11SelectionName(sel)))).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return reply.Owner
}

// clearX11 unsets an X11 selection the way a client clears its clipboard.
func clearX11(t *testing.T, sel selection) {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	atom := x11Atom(t, conn, strings.ToUpper(x11SelectionName(sel)))
	if err := xproto.SetSelectionOwnerChecked(conn, 0, atom, xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopBridge(t *testing.T) {
	binary := os.Getenv("CLIP_SYNC_BINARY")
	if binary == "" {
		t.Fatal("set CLIP_SYNC_BINARY and run through scripts/integration.sh")
	}
	logs := filepath.Join(t.TempDir(), "bridge.log")
	logFile, err := os.Create(logs)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	t.Cleanup(func() {
		if t.Failed() {
			output, _ := os.ReadFile(logs)
			t.Logf("bridge log:\n%s", output)
		}
	})
	d := &desktop{t: t}
	d.copy(waylandSide, []byte("initial Wayland"), "text/plain;charset=utf-8")
	d.await(waylandSide, []byte("initial Wayland"))
	d.copy(x11Side, []byte("initial X11"), "UTF8_STRING")
	d.await(x11Side, []byte("initial X11"))
	cmd := exec.Command(binary)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	process := startTestProcess(t, cmd)
	deadline := time.Now().Add(5 * time.Second)
	for {
		output, _ := os.ReadFile(logs)
		if strings.Contains(string(output), "clip-sync: ready") {
			break
		}
		select {
		case <-process.done:
			t.Fatalf("bridge exited: %v\n%s", process.err, output)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge startup timed out\n%s", output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.await(waylandSide, []byte("initial Wayland"))
	d.await(x11Side, []byte("initial X11"))
	for _, data := range [][]byte{
		[]byte("Unicode: café 日本語 🌍\nsecond line\r\n"),
		[]byte{},
		bytes.Repeat([]byte("large clipboard 🌍\n"), 65000),
	} {
		for _, from := range []side{waylandSide, x11Side} {
			mime := "text/plain;charset=utf-8"
			if from == x11Side {
				mime = "UTF8_STRING"
			}
			d.copy(from, data, mime)
			d.await(1-from, data)
		}
	}

	testX11Limits(t, logs)

	d.copy(x11Side, []byte{'c', 'a', 'f', 0xe9}, "STRING")
	d.await(waylandSide, []byte("café"))
	t.Run("paste to file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "paste.txt")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", "text")
		cmd.Stdout = file
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		for {
			got, err := os.ReadFile(path)
			if err == nil && string(got) == "café" {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("redirected paste did not reach the file")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	fresh := []byte("fresh text must survive delayed X11 data")
	requested, release := delayedX11Copy(t)
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not request the delayed X11 copy")
	}
	d.copy(waylandSide, fresh, "text/plain;charset=utf-8")
	d.await(x11Side, fresh)
	release()
	time.Sleep(100 * time.Millisecond)
	d.await(waylandSide, fresh)

	requested, release = delayedWaylandCopy(t)
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not request the delayed Wayland copy")
	}
	fresh = []byte("fresh text must survive delayed Wayland data")
	d.copy(x11Side, fresh, "UTF8_STRING")
	d.await(waylandSide, fresh)
	release()
	time.Sleep(100 * time.Millisecond)
	d.await(x11Side, fresh)

	d.copy(waylandSide, []byte("unsupported content"), "application/x-unsupported")
	time.Sleep(100 * time.Millisecond)
	d.await(x11Side, fresh)

	t.Run("formats", testFormats)
	t.Run("oversized format", testOversizedFormat)
	t.Run("stalled format", testStalledFormat)
	t.Run("large image", testLargeImage)
	t.Run("primary", testPrimary)
	t.Run("clears", testClears)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	duplicate, err := exec.CommandContext(ctx, binary).CombinedOutput()
	cancel()
	if err == nil || !strings.Contains(string(duplicate), "another clip-sync process") {
		t.Fatalf("duplicate process was not rejected: %v", err)
	}

	countFDs := func() int {
		t.Helper()
		entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", process.cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := countFDs()
	for i := range 300 {
		from := side(i % 2)
		mime := "text/plain;charset=utf-8"
		if from == x11Side {
			mime = "UTF8_STRING"
		}
		data := fmt.Appendf(nil, "clipboard-cycle-%d", i)
		d.copy(from, data, mime)
		d.await(1-from, data)
	}
	time.Sleep(200 * time.Millisecond)
	after := countFDs()
	if after > before+3 {
		t.Fatalf("descriptor count grew from %d to %d", before, after)
	}
	if value := os.Getenv("CLIP_SYNC_SOAK"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(duration)
		for time.Now().Before(until) {
			time.Sleep(time.Second)
			if countFDs() > before+3 {
				t.Fatal("descriptor count grew while idle")
			}
		}
	}
	output, _ := os.ReadFile(logs)
	if bytes.Contains(output, fresh) {
		t.Fatal("clipboard payload appeared in logs")
	}
	process.stop()
	if process.err != nil {
		t.Fatalf("bridge did not shut down cleanly: %v\n%s", process.err, output)
	}
	t.Logf("clipboard cycles completed; descriptors before=%d after=%d", before, after)
}

func testFormats(t *testing.T) {
	fromWayland := clip{
		textKey:         []byte("rich text from Wayland"),
		"text/html":     []byte("<b>rich</b> text from Wayland"),
		"text/uri-list": []byte("file:///tmp/clip-sync-wayland\r\n"),
		"image/png":     append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0, 1, 0xfe, 0xff}, 256)...),
	}
	ownWayland(t, selClipboard, []string{textKey, "text/html", "text/uri-list", "image/png"}, func(w *wayland, mime string, fd int) {
		w.serve(fd, fromWayland[mime])
	})
	awaitFormat(t, x11Side, selClipboard, "UTF8_STRING", fromWayland[textKey])
	for _, mime := range fromWayland.formats() {
		awaitFormat(t, x11Side, selClipboard, mime, fromWayland[mime])
	}

	fromX11 := map[string][]byte{
		"UTF8_STRING":                  []byte("rich text from X11"),
		"text/html":                    []byte("<i>rich</i> text from X11"),
		"x-special/gnome-copied-files": []byte("copy\nfile:///tmp/clip-sync-x11"),
		"image/png":                    append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0xff, 0, 7}, 256)...),
	}
	ownX11(t, selClipboard, fromX11, nil)
	awaitFormat(t, waylandSide, selClipboard, "text", fromX11["UTF8_STRING"])
	for _, mime := range []string{"text/html", "x-special/gnome-copied-files", "image/png"} {
		awaitFormat(t, waylandSide, selClipboard, mime, fromX11[mime])
	}
}

func testOversizedFormat(t *testing.T) {
	html := []byte("<p>valid after oversized text</p>")
	ownWayland(t, selClipboard, []string{textKey, "text/html"}, func(_ *wayland, mime string, fd int) {
		// Write in the event loop, one format at a time, like a blocking owner.
		f := os.NewFile(uintptr(fd), "paste")
		defer f.Close()
		if mime == textKey {
			f.Write(bytes.Repeat([]byte("x"), maxClipboardBytes+1<<20))
			return
		}
		f.Write(html)
	})
	awaitFormat(t, x11Side, selClipboard, "text/html", html)
}

func testStalledFormat(t *testing.T) {
	html := []byte("<p>sent although text stalls</p>")
	ownX11(t, selClipboard, map[string][]byte{"UTF8_STRING": []byte("never sent"), "text/html": html}, func(target string) bool {
		return target != "UTF8_STRING"
	})
	awaitFormat(t, waylandSide, selClipboard, "text/html", html)
}

func testLargeImage(t *testing.T) {
	d := &desktop{t: t}
	image := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0, 0xff, '\r', '\n'}, 200000)
	d.copy(waylandSide, image, "image/png")
	awaitFormat(t, x11Side, selClipboard, "image/png", image)
	image = bytes.Repeat([]byte{0, 'i', 'm', 'g', 0xff}, 300000)
	d.copy(x11Side, image, "image/png")
	awaitFormat(t, waylandSide, selClipboard, "image/png", image)
}

func testPrimary(t *testing.T) {
	d := &desktop{t: t}
	d.copy(waylandSide, []byte("clipboard stays"), textKey)
	d.await(x11Side, []byte("clipboard stays"))

	cmd := exec.Command("wl-copy", "--foreground", "--primary", "--type", textKey)
	cmd.Stdin = strings.NewReader("primary from Wayland")
	startTestProcess(t, cmd)
	awaitFormat(t, x11Side, selPrimary, "UTF8_STRING", []byte("primary from Wayland"))

	cmd = exec.Command("xclip", "-selection", "primary", "-in", "-quiet", "-target", "UTF8_STRING")
	cmd.Stdin = strings.NewReader("primary from X11")
	startTestProcess(t, cmd)
	awaitFormat(t, waylandSide, selPrimary, "text", []byte("primary from X11"))
	d.await(waylandSide, []byte("clipboard stays"))
	d.await(x11Side, []byte("clipboard stays"))

	if err := exec.Command("wl-copy", "--primary", "--clear").Run(); err != nil {
		t.Fatal(err)
	}
	awaitEmpty(t, x11Side, selPrimary)
	d.await(x11Side, []byte("clipboard stays"))
}

func testClears(t *testing.T) {
	d := &desktop{t: t}
	d.copy(waylandSide, []byte("cleared on Wayland"), textKey)
	d.await(x11Side, []byte("cleared on Wayland"))
	if err := exec.Command("wl-copy", "--clear").Run(); err != nil {
		t.Fatal(err)
	}
	awaitEmpty(t, x11Side, selClipboard)

	d.copy(x11Side, []byte("cleared on X11"), "UTF8_STRING")
	d.await(waylandSide, []byte("cleared on X11"))
	clearX11(t, selClipboard)
	awaitEmpty(t, waylandSide, selClipboard)

	d.copy(waylandSide, []byte("owner exits"), textKey)
	d.await(x11Side, []byte("owner exits"))
	d.owners[waylandSide].stop()
	awaitEmpty(t, x11Side, selClipboard)
}

// ownX11 owns an X11 selection with the given targets until the test ends.
// respond, if set, runs before each conversion other than TARGETS and can
// leave the request unanswered.
func ownX11(t *testing.T, sel selection, formats map[string][]byte, respond func(target string) bool) {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	selectionAtom, targets := x11Atom(t, conn, strings.ToUpper(x11SelectionName(sel))), x11Atom(t, conn, "TARGETS")
	offered := []xproto.Atom{targets}
	values, names := make(map[xproto.Atom][]byte), make(map[xproto.Atom]string)
	for name, value := range formats {
		atom := x11Atom(t, conn, name)
		offered = append(offered, atom)
		values[atom], names[atom] = value, name
	}
	window, err := xproto.NewWindowId(conn)
	if err != nil {
		t.Fatal(err)
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	if err := xproto.CreateWindowChecked(conn, 0, window, root, 0, 0, 1, 1, 0, xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { conn.Close(); <-done })
	go func() {
		defer close(done)
		for {
			event, err := conn.WaitForEvent()
			if event == nil || err != nil {
				return
			}
			e, ok := event.(xproto.SelectionRequestEvent)
			if !ok {
				continue
			}
			notify := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target}
			data, ok := values[e.Target]
			kind, format := e.Target, byte(8)
			switch {
			case e.Target == targets:
				kind, format, data = xproto.AtomAtom, 32, make([]byte, 4*len(offered))
				for i, atom := range offered {
					xgb.Put32(data[i*4:], uint32(atom))
				}
			case !ok:
				xproto.SendEventChecked(conn, false, e.Requestor, 0, string(notify.Bytes())).Check()
				continue
			case respond != nil && !respond(names[e.Target]):
				continue
			}
			if xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, e.Requestor, e.Property, kind, format, uint32(len(data)/(int(format)/8)), data).Check() == nil {
				notify.Property = e.Property
			}
			xproto.SendEventChecked(conn, false, e.Requestor, 0, string(notify.Bytes())).Check()
		}
	}()
	if err := xproto.SetSelectionOwnerChecked(conn, window, selectionAtom, xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
}

// ownWayland offers mimes on a Wayland selection until the test ends.
// send answers each paste request.
func ownWayland(t *testing.T, sel selection, mimes []string, send func(w *wayland, mime string, fd int)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &bridge{ctx: ctx, sends: make(chan struct{}, maxTransfers)}
	w, err := openWayland(b)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	started := false
	t.Cleanup(func() {
		cancel()
		if started {
			<-done
		}
		w.close()
		b.jobs.Wait()
	})
	source, err := w.manager.CreateDataSource()
	if err != nil {
		t.Fatal(err)
	}
	for _, mime := range mimes {
		if err := source.Offer(mime); err != nil {
			t.Fatal(err)
		}
	}
	source.SetSendHandler(func(e control.ExtDataControlSourceV1SendEvent) { send(w, e.MimeType, e.Fd) })
	source.SetCancelledHandler(func(control.ExtDataControlSourceV1CancelledEvent) { source.Destroy() })
	set := w.device.SetSelection
	if sel == selPrimary {
		set = w.device.SetPrimarySelection
	}
	if err := set(source); err != nil {
		t.Fatal(err)
	}
	w.start()
	started = true
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-w.events:
				if w.dispatch(event) != nil {
					return
				}
			}
		}
	}()
}

func delayedX11Copy(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	requested, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	once := sync.OnceFunc(func() { close(requested) })
	ownX11(t, selClipboard, map[string][]byte{"UTF8_STRING": []byte("stale X11 content")}, func(string) bool {
		once()
		<-gate
		return true
	})
	// Release first: the owner's cleanup waits for a conversion blocked on the gate.
	t.Cleanup(release)
	return requested, release
}

func delayedWaylandCopy(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	requested, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	once := sync.OnceFunc(func() { close(requested) })
	ownWayland(t, selClipboard, []string{textKey}, func(w *wayland, _ string, fd int) {
		once()
		<-gate
		w.serve(fd, []byte("stale Wayland content"))
	})
	// Release first: the owner's cleanup waits for a send handler blocked on the gate.
	t.Cleanup(release)
	return requested, release
}
