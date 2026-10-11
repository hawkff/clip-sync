package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AvengeMedia/dankgo/wayland/client"
	control "github.com/AvengeMedia/dankgo/wayland/ext_data_control"
	"golang.org/x/sys/unix"
)

var textMIMEs = []string{"text/plain;charset=utf-8", "text/plain;charset=UTF-8", "UTF8_STRING", "text/plain"}

type wlEvent struct {
	sender, opcode uint32
	fd             int
	data           []byte
	err            error
}

type wlOffer struct {
	proxy *control.ExtDataControlOfferV1
	mime  string
	own   string
}

type wayland struct {
	b          *bridge
	display    *client.Display
	manager    *control.ExtDataControlManagerV1
	device     *control.ExtDataControlDeviceV1
	offers     map[uint32]*wlOffer
	current    *wlOffer
	source     *control.ExtDataControlSourceV1
	pending    *client.Callback
	read       *os.File
	readCancel context.CancelFunc
	marker     string
	nonce      [16]byte
	serial     uint64
	err        error
	events     chan wlEvent
	done       chan struct{}
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
	if _, err = rand.Read(w.nonce[:]); err != nil {
		return nil, err
	}
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
		offer := &wlOffer{proxy: e.Id}
		w.offers[e.Id.ID()] = offer
		e.Id.SetOfferHandler(func(e control.ExtDataControlOfferV1OfferEvent) {
			if e.MimeType == w.marker && w.marker != "" {
				offer.own = e.MimeType
			}
			if mimeRank(e.MimeType) < mimeRank(offer.mime) {
				offer.mime = e.MimeType
			}
		})
	})
	w.device.SetSelectionHandler(func(e control.ExtDataControlDeviceV1SelectionEvent) { w.selection(e.Id) })
	w.device.SetPrimarySelectionHandler(func(e control.ExtDataControlDeviceV1PrimarySelectionEvent) {
		if e.Id != nil {
			w.destroyOffer(e.Id)
		}
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

func mimeRank(mime string) int {
	for i, candidate := range textMIMEs {
		if mime == candidate {
			return i
		}
	}
	return len(textMIMEs)
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

func (w *wayland) selection(proxy *control.ExtDataControlOfferV1) {
	w.cancelRead()
	if w.current != nil {
		w.destroyOffer(w.current.proxy)
		w.current = nil
	}
	if proxy != nil {
		w.current = w.offers[proxy.ID()]
	}
	if !w.b.ready {
		return
	}
	if w.current == nil {
		w.b.state.lost(waylandSide)
		return
	}
	if w.source != nil && w.current.own == w.marker && w.marker != "" {
		return
	}
	generation := w.b.changed(waylandSide)
	if w.current.mime == "" {
		return
	}
	r, writer, err := os.Pipe()
	if err != nil {
		w.fail(err)
		return
	}
	if err = r.SetReadDeadline(time.Now().Add(transferTimeout)); err != nil {
		r.Close()
		writer.Close()
		w.fail(err)
		return
	}
	err = proxy.Receive(w.current.mime, int(writer.Fd()))
	writer.Close()
	if err != nil {
		r.Close()
		w.fail(err)
		return
	}
	ctx, cancel := context.WithCancel(w.b.ctx)
	w.read, w.readCancel = r, cancel
	w.b.jobs.Add(1)
	go func() {
		defer w.b.jobs.Done()
		defer cancel()
		defer r.Close()
		stop := context.AfterFunc(ctx, func() { r.Close() })
		defer stop()
		data, err := readText(r, maxClipboardBytes)
		select {
		case w.b.results <- readResult{generation: generation, data: data, err: err}:
		case <-ctx.Done():
		}
	}()
}

func (w *wayland) cancelRead() {
	if w.readCancel != nil {
		w.readCancel()
		w.readCancel = nil
	}
	if w.read != nil {
		w.read.Close()
		w.read = nil
	}
}

func (w *wayland) publish(generation uint64, data []byte) error {
	if w.pending != nil {
		w.pending.Destroy()
	}
	callback, err := w.display.Sync()
	if err != nil {
		return err
	}
	w.pending = callback
	callback.SetDoneHandler(func(client.CallbackDoneEvent) {
		callback.Destroy()
		w.pending = nil
		// Process earlier compositor events before replacing its selection.
		if !w.b.state.current(generation) {
			return
		}
		w.fail(w.setSelection(data))
	})
	return nil
}

func (w *wayland) setSelection(data []byte) error {
	source, err := w.manager.CreateDataSource()
	if err != nil {
		return err
	}
	w.serial++
	marker := fmt.Sprintf("application/x-clip-sync-%x-%d", w.nonce, w.serial)
	for _, mime := range append(append([]string{}, textMIMEs...), marker) {
		if err := source.Offer(mime); err != nil {
			source.Destroy()
			return err
		}
	}
	source.SetSendHandler(func(e control.ExtDataControlSourceV1SendEvent) {
		payload := data
		if e.MimeType == marker {
			payload = nil
		} else if mimeRank(e.MimeType) == len(textMIMEs) {
			unix.Close(e.Fd)
			return
		}
		w.serve(e.Fd, payload)
	})
	source.SetCancelledHandler(func(control.ExtDataControlSourceV1CancelledEvent) {
		w.fail(source.Destroy())
		if w.source == source {
			w.source = nil
		}
	})
	if err := w.device.SetSelection(source); err != nil {
		source.Destroy()
		return err
	}
	if w.source != nil {
		if err := w.source.Destroy(); err != nil {
			return err
		}
	}
	w.source, w.marker = source, marker
	w.b.state.copied(waylandSide, data)
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
	w.cancelRead()
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
