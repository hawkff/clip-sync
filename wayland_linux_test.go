package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testWayland(t *testing.T) (*wayland, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &wayland{b: &bridge{ctx: ctx, sends: make(chan struct{}, maxTransfers)}}
	t.Cleanup(func() {
		cancel()
		done := make(chan struct{})
		go func() { w.b.jobs.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("clipboard workers did not stop")
		}
	})
	return w, cancel
}

func testPipe(t *testing.T) (*os.File, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(int(w.Fd()))
	w.Close()
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err := r.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return r, fd
}

func TestWaylandServeLargeText(t *testing.T) {
	w, _ := testWayland(t)
	r, fd := testPipe(t)
	data := bytes.Repeat([]byte("large clipboard\n"), 100000)
	w.serve(fd, data)
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("large transfer failed: length=%d error=%v", len(got), err)
	}
}

func TestWaylandServeRegularFile(t *testing.T) {
	w, _ := testWayland(t)
	f, err := os.CreateTemp(t.TempDir(), "paste-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	w.serve(fd, []byte("redirected paste"))
	w.b.jobs.Wait()
	got, err := os.ReadFile(f.Name())
	if err != nil || string(got) != "redirected paste" {
		t.Fatalf("regular-file paste failed: length=%d error=%v", len(got), err)
	}
}

func TestWaylandSlowReaderDoesNotBlockOtherPastes(t *testing.T) {
	w, cancel := testWayland(t)
	_, blockedFD := testPipe(t)
	w.serve(blockedFD, bytes.Repeat([]byte("x"), 1<<20))
	r, fd := testPipe(t)
	w.serve(fd, []byte("fresh"))
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "fresh" {
		t.Fatalf("slow reader blocked another paste: %v", err)
	}
	cancel()
}

func TestWaylandSendLimitClosesRejectedDescriptor(t *testing.T) {
	w, _ := testWayland(t)
	for range maxTransfers {
		w.b.sends <- struct{}{}
	}
	r, fd := testPipe(t)
	w.serve(fd, []byte("ignored"))
	got, err := io.ReadAll(r)
	if err != nil || len(got) != 0 {
		t.Fatalf("rejected transfer did not close: %v", err)
	}
}

func TestWaylandRepeatedPastesCloseDescriptors(t *testing.T) {
	w, _ := testWayland(t)
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for range 300 {
		r, fd := testPipe(t)
		w.serve(fd, []byte("text"))
		if _, err := io.ReadAll(r); err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	w.b.jobs.Wait()
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before)+1 {
		t.Fatalf("descriptors grew from %d to %d", len(before), len(after))
	}
}
