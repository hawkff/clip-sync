//go:build integration

package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func testX11Limits(t *testing.T, logs string) {
	t.Helper()
	for _, test := range []struct {
		name          string
		target        string
		incremental   bool
		advertiseSize bool
		fill          byte
		size          int
	}{
		{"direct property", "UTF8_STRING", false, false, 'x', maxClipboardBytes},
		{"INCR announced size", "UTF8_STRING", true, true, 'x', maxClipboardBytes},
		{"INCR streamed size", "UTF8_STRING", true, false, 'x', maxClipboardBytes},
		{"STRING expansion", "STRING", true, true, 0xe9, maxClipboardBytes / 2},
	} {
		t.Run("X11 limit/"+test.name, func(t *testing.T) {
			d := &desktop{t: t}
			valid := bytes.Repeat([]byte{test.fill}, test.size)
			want := valid
			if test.target == "STRING" {
				want = bytes.Repeat([]byte("é"), test.size)
			}
			stopValid := serveX11LimitSelection(t, valid, test.target, test.incremental, test.advertiseSize, nil)
			d.await(waylandSide, want)

			failures := func() int {
				output, err := os.ReadFile(logs)
				if err != nil {
					t.Fatal(err)
				}
				return strings.Count(string(output), "clip-sync: ignored failed X11 transfer")
			}
			before := failures()
			oversized := bytes.Repeat([]byte{test.fill}, test.size+1)
			stop := serveX11LimitSelection(t, oversized, test.target, test.incremental, test.advertiseSize, nil)
			// The oversized owner replaced this one, so closing it does not empty the clipboard.
			stopValid()
			// Require rejection before the ten-second transfer timeout can mask a stalled fixture.
			deadline := time.Now().Add(5 * time.Second)
			for failures() == before {
				if time.Now().After(deadline) {
					t.Fatal("oversized selection was not rejected")
				}
				time.Sleep(10 * time.Millisecond)
			}
			d.await(waylandSide, want)
			stop()

			recovery := []byte("valid copy after " + test.name)
			d.copy(x11Side, recovery, "UTF8_STRING")
			d.await(waylandSide, recovery)
		})
	}
	t.Run("X11 limit/format after rejected INCR", func(t *testing.T) {
		// The owner sends the HTML only after the bridge has taken every
		// chunk of the text it rejected.
		html := []byte("<p>sent after oversized text</p>")
		oversized := bytes.Repeat([]byte("x"), maxClipboardBytes+1)
		stop := serveX11LimitSelection(t, oversized, "UTF8_STRING", true, true, html)
		awaitFormat(t, waylandSide, selClipboard, "text/html", html)
		stop()
	})
}

// serveX11LimitSelection owns the X11 clipboard with data as targetName. With
// html it also offers text/html, and like an owner that serves one transfer
// at a time, it answers that request only once no INCR transfer is running.
func serveX11LimitSelection(t *testing.T, data []byte, targetName string, incremental, advertiseSize bool, html []byte) func() {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	closing := make(chan struct{})
	var done chan struct{}
	stop := sync.OnceFunc(func() {
		close(closing)
		conn.Close()
		if done != nil {
			<-done
		}
	})
	t.Cleanup(stop)
	clipboard, targets, text, incr := x11Atom(t, conn, "CLIPBOARD"), x11Atom(t, conn, "TARGETS"), x11Atom(t, conn, targetName), x11Atom(t, conn, "INCR")
	htmlAtom := x11Atom(t, conn, "text/html")
	window, err := xproto.NewWindowId(conn)
	if err != nil {
		t.Fatal(err)
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	if err := xproto.CreateWindowChecked(conn, 0, window, root, 0, 0, 1, 1, 0, xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		t.Fatal(err)
	}
	chunkSize := min(64<<10, int(xproto.Setup(conn).MaximumRequestLength)*4-24)
	if chunkSize <= 0 {
		t.Fatal("invalid X11 request size")
	}
	failed := func(err error) bool {
		if err == nil {
			return false
		}
		select {
		case <-closing:
		default:
			// The bridge keeps a requestor window while its owner may still write to it.
			t.Errorf("X11 selection fixture: %v", err)
		}
		return true
	}
	sendHTML := func(e xproto.SelectionRequestEvent) {
		notify := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target}
		if !failed(xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, e.Requestor, e.Property, htmlAtom, 8, uint32(len(html)), html).Check()) {
			notify.Property = e.Property
		}
		failed(xproto.SendEventChecked(conn, false, e.Requestor, 0, string(notify.Bytes())).Check())
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		var requestor xproto.Window
		var property xproto.Atom
		var remaining []byte
		var waiting *xproto.SelectionRequestEvent
		for {
			event, err := conn.WaitForEvent()
			if err != nil {
				failed(err)
				return
			}
			if event == nil {
				return
			}
			switch e := event.(type) {
			case xproto.SelectionRequestEvent:
				notify := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target}
				prop := e.Property
				if prop == 0 {
					prop = e.Target
				}
			prepareReply:
				switch e.Target {
				case targets:
					offered := []xproto.Atom{text}
					if html != nil {
						offered = append(offered, htmlAtom)
					}
					value := make([]byte, 4*len(offered))
					for i, atom := range offered {
						xgb.Put32(value[i*4:], uint32(atom))
					}
					if !failed(xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, uint32(len(offered)), value).Check()) {
						notify.Property = prop
					}
				case htmlAtom:
					if requestor != 0 {
						waiting = &e
					} else {
						sendHTML(e)
					}
					continue
				case text:
					if incremental {
						if failed(xproto.ChangeWindowAttributesChecked(conn, e.Requestor, xproto.CwEventMask, []uint32{xproto.EventMaskPropertyChange}).Check()) {
							continue
						}
						value := make([]byte, 4)
						size := uint32(1)
						if advertiseSize {
							size = uint32(len(data))
						}
						xgb.Put32(value, size)
						if !failed(xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, e.Requestor, prop, incr, 32, 1, value).Check()) {
							requestor, property, remaining = e.Requestor, prop, data
							notify.Property = prop
						}
					} else {
						// Append requests create a large non-INCR property without BIG-REQUESTS.
						mode := byte(xproto.PropModeReplace)
						for offset := 0; offset < len(data); offset += chunkSize {
							chunk := data[offset:min(offset+chunkSize, len(data))]
							if failed(xproto.ChangePropertyChecked(conn, mode, e.Requestor, prop, text, 8, uint32(len(chunk)), chunk).Check()) {
								break prepareReply
							}
							mode = xproto.PropModeAppend
						}
						notify.Property = prop
					}
				}
				failed(xproto.SendEventChecked(conn, false, e.Requestor, 0, string(notify.Bytes())).Check())
			case xproto.PropertyNotifyEvent:
				if requestor == 0 || e.Window != requestor || e.Atom != property || e.State != xproto.PropertyDelete {
					continue
				}
				n := min(len(remaining), chunkSize)
				if failed(xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, requestor, property, text, 8, uint32(n), remaining[:n]).Check()) || n == 0 {
					requestor, remaining = 0, nil
					if waiting != nil {
						sendHTML(*waiting)
						waiting = nil
					}
					continue
				}
				remaining = remaining[n:]
			}
		}
	}()
	if err := xproto.SetSelectionOwnerChecked(conn, window, clipboard, xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
	return stop
}
