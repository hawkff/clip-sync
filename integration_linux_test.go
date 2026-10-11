//go:build integration

package main

import (
	"bytes"
	"context"
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

func (d *desktop) paste(from side) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if from == waylandSide {
		cmd = exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", "text")
	} else {
		cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-out", "-target", "UTF8_STRING")
	}
	// A Wayland source can retain the stdout descriptor after wl-paste exits.
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.Output()
}

func (d *desktop) await(from side, want []byte) {
	d.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var got []byte
	var err error
	for time.Now().Before(deadline) {
		got, err = d.paste(from)
		if err == nil && bytes.Equal(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.t.Fatalf("clipboard %d mismatch: got %d bytes, want %d, error=%v", from, len(got), len(want), err)
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

	d.copy(waylandSide, []byte("not a text selection"), "image/png")
	time.Sleep(100 * time.Millisecond)
	d.await(x11Side, fresh)
	if err := exec.Command("wl-copy", "--clear").Run(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	d.await(x11Side, fresh)

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

func delayedX11Copy(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	atom := func(name string) xproto.Atom {
		reply, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
		if err != nil {
			t.Fatal(err)
		}
		return reply.Atom
	}
	clipboard, targets, text := atom("CLIPBOARD"), atom("TARGETS"), atom("UTF8_STRING")
	window, err := xproto.NewWindowId(conn)
	if err != nil {
		t.Fatal(err)
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	if err := xproto.CreateWindowChecked(conn, 0, window, root, 0, 0, 1, 1, 0, xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		t.Fatal(err)
	}
	requested, gate, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	once := sync.OnceFunc(func() { close(requested) })
	t.Cleanup(func() { release(); conn.Close(); <-done })
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
			var data []byte
			kind, format := text, byte(8)
			switch e.Target {
			case targets:
				kind, format, data = xproto.AtomAtom, 32, make([]byte, 4)
				xgb.Put32(data, uint32(text))
			case text:
				once()
				<-gate
				data = []byte("stale X11 content")
			default:
				xproto.SendEventChecked(conn, false, e.Requestor, 0, string(notify.Bytes())).Check()
				continue
			}
			if xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, e.Requestor, e.Property, kind, format, uint32(len(data)/(int(format)/8)), data).Check() == nil {
				notify.Property = e.Property
			}
			xproto.SendEventChecked(conn, false, e.Requestor, 0, string(notify.Bytes())).Check()
		}
	}()
	if err := xproto.SetSelectionOwnerChecked(conn, window, clipboard, xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
	return requested, release
}

func delayedWaylandCopy(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &bridge{ctx: ctx, sends: make(chan struct{}, maxTransfers)}
	w, err := openWayland(b)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	requested, gate, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	once := sync.OnceFunc(func() { close(requested) })
	started := false
	t.Cleanup(func() {
		release()
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
	if err := source.Offer("text/plain;charset=utf-8"); err != nil {
		t.Fatal(err)
	}
	source.SetSendHandler(func(e control.ExtDataControlSourceV1SendEvent) {
		once()
		<-gate
		w.serve(e.Fd, []byte("stale Wayland content"))
	})
	source.SetCancelledHandler(func(control.ExtDataControlSourceV1CancelledEvent) { source.Destroy() })
	if err := w.device.SetSelection(source); err != nil {
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
	return requested, release
}
