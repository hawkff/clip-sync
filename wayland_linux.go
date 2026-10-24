package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AvengeMedia/dankgo/wayland/client"
	control "github.com/AvengeMedia/dankgo/wayland/ext_data_control"
	"golang.org/x/sys/unix"
)

type wlEvent struct {
	sender, opcode uint32
	fd             int
	data           []byte
	err            error
}

type wlOffer struct {
	proxy   *control.ExtDataControlOfferV1
	offered map[string]bool
	own     string
}

// wlSelection tracks one Wayland selection: CLIPBOARD or PRIMARY.
type wlSelection struct {
	current    *wlOffer
	source     *control.ExtDataControlSourceV1
	marker     string
	pending    *client.Callback
	readCancel context.CancelFunc
	// unsets counts our unset requests whose selection events have not arrived.
	unsets int
}

type wayland struct {
	b       *bridge
	display *client.Display
	manager *control.ExtDataControlManagerV1
	device  *control.ExtDataControlDeviceV1
	offers  map[uint32]*wlOffer
	sel     [2]wlSelection
	primary bool
	prefix  string
	serial  uint64
	err     error
	events  chan wlEvent
	done    chan struct{}
}

func openWayland(b *bridge) (_ *wayland, err error) {
	path := os.Getenv("WAYLAND_DISPLAY")
	if !filepath.IsAbs(path) {
		path = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), path)
	}
	display, err := client.Connect(path)
	if err != nil {
		return nil, err
	}
	w := &wayland{b: b, display: display, offers: make(map[uint32]*wlOffer), events: make(chan wlEvent, 32)}
	defer func() {
		if err != nil {
			display.Context().Close()
		}
	}()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	w.prefix = fmt.Sprintf("application/x-clip-sync-%x-", nonce)
	ctx := display.Context()
	if err = ctx.SetReadDeadline(time.Now().Add(transferTimeout)); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(b.ctx, func() { ctx.SetReadDeadline(time.Now()) })
	defer stop()
	display.SetErrorHandler(func(e client.DisplayErrorEvent) {
		w.fail(fmt.Errorf("compositor protocol error %d", e.Code))
	})
	display.SetDeleteIdHandler(func(e client.DisplayDeleteIdEvent) { ctx.DeleteID(e.Id) })
	registry, err := display.GetRegistry()
	if err != nil {
		return nil, err
	}
	var seats []client.RegistryGlobalEvent
	var ext, legacy client.RegistryGlobalEvent
	registry.SetGlobalHandler(func(e client.RegistryGlobalEvent) {
		switch e.Interface {
		case "wl_seat":
			seats = append(seats, e)
		case control.ExtDataControlManagerV1InterfaceName:
			ext = e
		case "zwlr_data_control_manager_v1":
			legacy = e
		}
	})
	if err = display.Roundtrip(); err != nil {
		return nil, err
	}
	if len(seats) != 1 {
		return nil, fmt.Errorf("expected one Wayland seat, found %d", len(seats))
	}
	global, version := ext, uint32(1)
	if global.Interface == "" {
		global, version = legacy, min(legacy.Version, 2)
	}
	if global.Interface == "" {
		return nil, errors.New("compositor does not advertise ext-data-control or wlr-data-control")
	}
	registry.SetGlobalRemoveHandler(func(e client.RegistryGlobalRemoveEvent) {
		if e.Name == seats[0].Name || e.Name == global.Name {
			w.fail(errors.New("clipboard manager or seat disappeared"))
		}
	})
	seat := client.NewSeat(ctx)
	if err = registry.Bind(seats[0].Name, "wl_seat", 1, seat); err != nil {
		return nil, err
	}
	// ext v1 and wlr v2 share these request and event layouts.
	w.manager = control.NewExtDataControlManagerV1(ctx)
	if err = registry.Bind(global.Name, global.Interface, version, w.manager); err != nil {
		return nil, err
	}
	w.device, err = w.manager.GetDataDevice(seat)
	if err != nil {
		return nil, err
	}
	w.device.SetDataOfferHandler(func(e control.ExtDataControlDeviceV1DataOfferEvent) {
		offer := &wlOffer{proxy: e.Id, offered: make(map[string]bool)}
		w.offers[e.Id.ID()] = offer
		e.Id.SetOfferHandler(func(e control.ExtDataControlOfferV1OfferEvent) {
			switch {
			case strings.HasPrefix(e.MimeType, w.prefix):
				offer.own = e.MimeType
			case slices.Contains(textMIMEs, e.MimeType), slices.Contains(extraMIMEs, e.MimeType), slices.Contains(imageMIMEs, e.MimeType):
				offer.offered[e.MimeType] = true
			}
		})
	})
	w.device.SetSelectionHandler(func(e control.ExtDataControlDeviceV1SelectionEvent) { w.selection(selClipboard, e.Id) })
	// Compositors with primary selection support send this event once on bind.
	w.device.SetPrimarySelectionHandler(func(e control.ExtDataControlDeviceV1PrimarySelectionEvent) {
		w.primary = true
		w.selection(selPrimary, e.Id)
	})
	w.device.SetFinishedHandler(func(control.ExtDataControlDeviceV1FinishedEvent) {
		w.fail(errors.New("data-control device finished"))
	})
	if err = display.Roundtrip(); err != nil {
		return nil, err
	}
	if w.err != nil {
		return nil, w.err
	}
	if err = b.ctx.Err(); err != nil {
		return nil, err
	}
	if err = ctx.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *wayland) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

func (w *wayland) destroyOffer(offer *control.ExtDataControlOfferV1) {
	w.fail(offer.Destroy())
	delete(w.offers, offer.ID())
	// The server allocates offer IDs and does not send delete_id for them.
	w.display.Context().Unregister(offer)
}

func (w *wayland) selection(sel selection, proxy *control.ExtDataControlOfferV1) {
	s := &w.sel[sel]
	w.cancelRead(sel)
	if s.current != nil {
		w.destroyOffer(s.current.proxy)
		s.current = nil
	}
	if proxy != nil {
		s.current = w.offers[proxy.ID()]
		if s.current == nil {
			s.current = &wlOffer{proxy: proxy, offered: make(map[string]bool)}
		}
	}
	if !w.b.ready {
		return
	}
	if s.current == nil && s.unsets > 0 {
		s.unsets--
		return
	}
	if s.current != nil && s.marker != "" && s.current.own == s.marker {
		// Our copy, or a clipboard manager's re-copy that kept our marker type.
		return
	}
	// Any other change ends our copy, so restoring it later syncs as a new copy.
	s.marker = ""
	if s.current == nil {
		w.fail(w.b.emptied(sel, waylandSide))
		return
	}
	generation := w.b.changed(sel, waylandSide)
	mimes := wanted(s.current.offered, textMIMEs)
	if len(mimes) == 0 {
		// Nothing to copy; the other clipboard keeps its content.
		w.b.state[sel].failed(generation)
		return
	}
	w.read(sel, generation, s.current.proxy, mimes)
}

// read receives every wanted format at once, so an owner that writes them in
// any order cannot stall the transfer.
func (w *wayland) read(sel selection, generation uint64, proxy *control.ExtDataControlOfferV1, mimes []string) {
	pipes := make([]*os.File, 0, len(mimes))
	closePipes := func() {
		for _, r := range pipes {
			r.Close()
		}
	}
	for _, mime := range mimes {
		r, writer, err := os.Pipe()
		if err != nil {
			closePipes()
			w.fail(err)
			return
		}
		pipes = append(pipes, r)
		if err = r.SetReadDeadline(time.Now().Add(transferTimeout)); err == nil {
			err = proxy.Receive(mime, int(writer.Fd()))
		}
		writer.Close()
		if err != nil {
			closePipes()
			w.fail(err)
			return
		}
	}
	ctx, cancel := context.WithCancel(w.b.ctx)
	w.sel[sel].readCancel = cancel
	w.b.jobs.Add(1)
	go func() {
		defer w.b.jobs.Done()
		defer cancel()
		defer closePipes()
		stop := context.AfterFunc(ctx, closePipes)
		defer stop()
		data := make(clip)
		var mu sync.Mutex
		var formats sync.WaitGroup
		for i, mime := range mimes {
			formats.Go(func() {
				// Closing early lets an owner that writes one format at a time move on.
				defer pipes[i].Close()
				key, read := mime, readData
				if slices.Contains(textMIMEs, mime) {
					key, read = textKey, readText
				}
				value, err := read(pipes[i], maxClipboardBytes)
				if err != nil {
					return
				}
				mu.Lock()
				data[key] = value
				mu.Unlock()
			})
		}
		formats.Wait()
		var err error
		if len(data) == 0 {
			err = errors.New("no clipboard format could be read")
		}
		select {
		case w.b.results <- readResult{sel: sel, generation: generation, data: data, err: err}:
		case <-ctx.Done():
		}
	}()
}

func (w *wayland) cancelRead(sel selection) {
	s := &w.sel[sel]
	if s.readCancel != nil {
		s.readCancel()
		s.readCancel = nil
	}
}

func (w *wayland) publish(sel selection, generation uint64, data clip) error {
	s := &w.sel[sel]
	if s.pending != nil {
		s.pending.Destroy()
	}
	callback, err := w.display.Sync()
	if err != nil {
		return err
	}
	s.pending = callback
	callback.SetDoneHandler(func(client.CallbackDoneEvent) {
		callback.Destroy()
		s.pending = nil
		// Process earlier compositor events before replacing its selection.
		if !w.b.state[sel].current(generation) {
			return
		}
		w.fail(w.setSelection(sel, data))
	})
	return nil
}

func (w *wayland) setSelection(sel selection, data clip) error {
	s := &w.sel[sel]
	set := w.device.SetSelection
	if sel == selPrimary {
		set = w.device.SetPrimarySelection
	}
	if len(data) == 0 {
		return w.unset(sel, set)
	}
	source, err := w.manager.CreateDataSource()
	if err != nil {
		return err
	}
	w.serial++
	marker := fmt.Sprintf("%s%d", w.prefix, w.serial)
	mimes := data.formats()
	if _, ok := data[textKey]; ok {
		mimes = append(slices.Clone(textMIMEs), mimes...)
	}
	for _, mime := range append(mimes, marker) {
		if err := source.Offer(mime); err != nil {
			source.Destroy()
			return err
		}
	}
	source.SetSendHandler(func(e control.ExtDataControlSourceV1SendEvent) {
		var payload []byte // the marker type carries no data
		if e.MimeType != marker {
			key := e.MimeType
			if slices.Contains(textMIMEs, key) {
				key = textKey
			}
			value, ok := data[key]
			if !ok {
				unix.Close(e.Fd)
				return
			}
			payload = value
		}
		w.serve(e.Fd, payload)
	})
	source.SetCancelledHandler(func(control.ExtDataControlSourceV1CancelledEvent) {
		w.fail(source.Destroy())
		if s.source == source {
			s.source = nil
		}
	})
	if err := set(source); err != nil {
		source.Destroy()
		return err
	}
	if s.source != nil {
		if err := s.source.Destroy(); err != nil {
			return err
		}
	}
	s.source, s.marker = source, marker
	w.b.state[sel].copied(waylandSide, data)
	return nil
}

// unset clears a Wayland selection to mirror an empty X11 clipboard.
func (w *wayland) unset(sel selection, set func(*control.ExtDataControlSourceV1) error) error {
	s := &w.sel[sel]
	if s.current != nil {
		// The compositor reports the unset back as a selection event.
		s.unsets++
		if err := set(nil); err != nil {
			return err
		}
	}
	if s.source != nil {
		if err := s.source.Destroy(); err != nil {
			return err
		}
	}
	s.source, s.marker = nil, ""
	w.b.state[sel].copied(waylandSide, nil)
	return nil
}

func (w *wayland) serve(fd int, data []byte) {
	if fd < 0 {
		w.fail(errors.New("missing clipboard transfer descriptor"))
		return
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return
	}
	f := os.NewFile(uintptr(fd), "clipboard transfer")
	select {
	case w.b.sends <- struct{}{}:
	default:
		f.Close()
		return
	}
	w.b.jobs.Add(1)
	go func() {
		defer w.b.jobs.Done()
		defer func() { <-w.b.sends }()
		defer f.Close()
		stop := context.AfterFunc(w.b.ctx, func() { f.Close() })
		defer stop()
		if err := f.SetWriteDeadline(time.Now().Add(transferTimeout)); err != nil && !errors.Is(err, os.ErrNoDeadline) {
			return
		}
		// A reader may abandon a paste without invalidating the selection.
		f.Write(data)
	}()
}

func (w *wayland) start() {
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		for {
			sender, opcode, fd, data, err := w.display.Context().ReadMsg()
			event := wlEvent{sender: sender, opcode: opcode, fd: fd, data: data, err: err}
			select {
			case w.events <- event:
			case <-w.b.ctx.Done():
				if fd >= 0 {
					unix.Close(fd)
				}
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

func (w *wayland) dispatch(event wlEvent) (err error) {
	if event.err != nil {
		return event.err
	}
	proxy := w.display.Context().GetProxy(event.sender)
	if proxy == nil || proxy.IsZombie() {
		if event.fd >= 0 {
			unix.Close(event.fd)
		}
		return nil
	}
	dispatcher, ok := proxy.(client.Dispatcher)
	if !ok {
		if event.fd >= 0 {
			unix.Close(event.fd)
		}
		return errors.New("unexpected Wayland event recipient")
	}
	defer func() {
		if recover() != nil {
			if event.fd >= 0 {
				unix.Close(event.fd)
			}
			err = errors.New("invalid Wayland event")
		}
	}()
	dispatcher.Dispatch(event.opcode, event.fd, event.data)
	return w.err
}

func (w *wayland) close() {
	for sel := range w.sel {
		w.cancelRead(selection(sel))
	}
	if w.done != nil {
		w.display.Context().SetReadDeadline(time.Now())
		<-w.done
	}
	for {
		select {
		case event := <-w.events:
			if event.fd >= 0 {
				unix.Close(event.fd)
			}
		default:
			w.display.Context().Close()
			return
		}
	}
}
