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

type bridge struct {
	ctx     context.Context
	state   [2]syncState
	ready   bool
	wl      *wayland
	x       *x11
	results chan readResult
	jobs    sync.WaitGroup
	sends   chan struct{}
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

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
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
			state := &b.state[result.sel]
			if !state.current(result.generation) {
				continue
			}
			if result.err != nil {
				state.failed(result.generation)
				log.Print("clip-sync: ignored failed Wayland transfer")
				continue
			}
			if err := b.received(result.sel, waylandSide, result.generation, result.data); err != nil {
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
	return sel == selClipboard || b.wl != nil && b.wl.primary
}

func (b *bridge) changed(sel selection, from side) uint64 {
	if b.wl != nil {
		b.wl.cancelRead(sel)
	}
	b.x.cancelRead(sel)
	return b.state[sel].changed(from)
}

func (b *bridge) received(sel selection, from side, generation uint64, data clip) error {
	if !b.state[sel].received(from, generation, data) {
		return nil
	}
	return b.publish(sel, 1-from, generation, data)
}

// emptied mirrors a clipboard that lost its owner on side from.
func (b *bridge) emptied(sel selection, from side) error {
	generation, mirror := b.state[sel].emptied(from)
	if !mirror {
		return nil
	}
	return b.publish(sel, 1-from, generation, nil)
}

func (b *bridge) publish(sel selection, to side, generation uint64, data clip) error {
	if to == x11Side {
		return b.x.publish(sel, generation, data)
	}
	return b.wl.publish(sel, generation, data)
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
