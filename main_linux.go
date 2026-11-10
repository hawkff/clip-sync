package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type readResult struct {
	sel        selection
	generation uint64
	data       clip
	err        error
}

// held is an X11 copy waiting out settleDelay before it goes to Wayland.
type held struct {
	generation uint64
	data       clip
}

type bridge struct {
	ctx     context.Context
	state   [2]syncState
	ready   bool
	wl      *wayland
	x       *x11
	results chan readResult
	jobs    sync.WaitGroup
	sends   chan struct{}
	held    [2]held
	settle  [2]*time.Timer
}

func main() {
	log.SetFlags(0)
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: clip-sync\n\nBridge the current Wayland and X11 clipboards.\nUses WAYLAND_DISPLAY, XDG_RUNTIME_DIR, DISPLAY, and XAUTHORITY.")
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Printf("clip-sync: %v", err)
		os.Exit(1)
	}
}

func run(parent context.Context) error {
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("%s is not set; run inside the desktop session", key)
		}
	}
	lock, err := lockSession()
	if err != nil {
		return err
	}
	defer lock.Close()

	ctx, cancel := context.WithCancel(parent)
	b := &bridge{ctx: ctx, results: make(chan readResult, maxTransfers), sends: make(chan struct{}, maxTransfers)}
	for i := range b.settle {
		b.settle[i] = time.NewTimer(settleDelay)
		b.settle[i].Stop()
	}
	defer func() {
		cancel()
		if b.wl != nil {
			b.wl.close()
		}
		if b.x != nil {
			b.x.close()
		}
		b.jobs.Wait()
	}()

	b.x, err = openX11(b)
	if err != nil {
		return fmt.Errorf("X11: %w", err)
	}
	b.wl, err = openWayland(b)
	if err != nil {
		return fmt.Errorf("Wayland: %w", err)
	}
	// Establish the startup baseline without overwriting either clipboard.
	b.x.conn.Sync()
	for {
		event, xerr := b.x.conn.PollForEvent()
		if xerr != nil {
			return xerr
		}
		if event == nil {
			break
		}
		if err := b.x.handle(event); err != nil {
			return err
		}
	}
	b.ready = true
	b.x.start()
	b.wl.start()
	log.Print("clip-sync: ready")

	// The ticker runs only while an X11 transfer can time out.
	const period = 100 * time.Millisecond
	tick := time.NewTicker(period)
	defer tick.Stop()
	ticking := true
	for {
		if busy := b.x.busy(); busy != ticking {
			ticking = busy
			if busy {
				tick.Reset(period)
			} else {
				tick.Stop()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case event := <-b.wl.events:
			if err := b.wl.dispatch(event); err != nil {
				return fmt.Errorf("Wayland: %w", err)
			}
		case event := <-b.x.events:
			if event.err != nil {
				return fmt.Errorf("X11: %w", event.err)
			}
			if err := b.x.handle(event.event); err != nil {
				return fmt.Errorf("X11: %w", err)
			}
		case result := <-b.results:
			var err error
			if result.err != nil {
				err = b.failed(result.sel, waylandSide, result.generation, true)
			} else {
				err = b.received(result.sel, waylandSide, result.generation, result.data)
			}
			if err != nil {
				return err
			}
		case <-b.settle[selClipboard].C:
			if err := b.release(selClipboard); err != nil {
				return err
			}
		case <-b.settle[selPrimary].C:
			if err := b.release(selPrimary); err != nil {
				return err
			}
		case now := <-tick.C:
			if err := b.x.expire(now); err != nil {
				return fmt.Errorf("X11: %w", err)
			}
		}
	}
}

// active reports whether the bridge syncs sel. PRIMARY needs compositor support.
func (b *bridge) active(sel selection) bool {
	return sel == selClipboard || b.wl.primary
}

func (b *bridge) changed(sel selection, from side) uint64 {
	b.wl.cancelRead(sel)
	b.x.cancelRead(sel)
	return b.state[sel].changed(from)
}

func (b *bridge) received(sel selection, from side, generation uint64, data clip) error {
	return b.next(sel, b.state[sel].received(from, generation, data))
}

// failed handles a read of side from that produced nothing to copy. transfer
// tells a failed transfer from a selection without a format the bridge copies.
func (b *bridge) failed(sel selection, from side, generation uint64, transfer bool) error {
	state := &b.state[sel]
	current := state.current(generation)
	st := state.failed(from, generation)
	// A failed read that the other side's copy then replaces is not worth a log line.
	if transfer && current && st.kind == stepNone {
		log.Printf("clip-sync: ignored failed %s transfer", from)
	}
	return b.next(sel, st)
}

// emptied handles a clipboard that lost its owner on side from.
func (b *bridge) emptied(sel selection, from side) error {
	return b.next(sel, b.state[sel].emptied(from))
}

func (b *bridge) next(sel selection, st step) error {
	generation := b.state[sel].generation
	switch st.kind {
	case stepPublish:
		return b.publish(sel, st.to, generation, st.data)
	case stepVerify:
		return b.read(sel, st.to, generation, verifyTimeout)
	case stepReread:
		return b.read(sel, st.to, b.changed(sel, st.to), transferTimeout)
	}
	return nil
}

// read starts reading the current content of side from for generation.
func (b *bridge) read(sel selection, from side, generation uint64, timeout time.Duration) error {
	if from == x11Side {
		return b.x.read(sel, generation, timeout)
	}
	return b.wl.read(sel, generation, timeout)
}

func (b *bridge) publish(sel selection, to side, generation uint64, data clip) error {
	switch {
	case to == x11Side:
		return b.x.publish(sel, generation, data)
	case len(data) == 0:
		return b.wl.publish(sel, generation, data)
	}
	// A compositor that bridges X11 itself is copying the same selection to
	// Wayland right now. Let its copy show up first, so the two do not race.
	b.held[sel] = held{generation, data}
	b.settle[sel].Reset(settleDelay)
	return nil
}

// release publishes the copy held for sel, unless a later change replaced it.
func (b *bridge) release(sel selection) error {
	h := b.held[sel]
	b.held[sel] = held{}
	if h.data == nil || !b.state[sel].current(h.generation) {
		return nil
	}
	return b.wl.publish(sel, h.generation, h.data)
}

func lockSession() (*os.File, error) {
	key := sha256.Sum256([]byte(os.Getenv("WAYLAND_DISPLAY") + "\x00" + os.Getenv("DISPLAY")))
	path := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), fmt.Sprintf("clip-sync-%x.lock", key[:8]))
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), "session lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("another clip-sync process is using this session")
		}
		return nil, fmt.Errorf("lock session: %w", err)
	}
	return f, nil
}
