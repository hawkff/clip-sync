package main

import (
	"errors"
	"io"
	"log"
	"time"
	"unicode/utf8"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xcmisc"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

type xEvent struct {
	event xgb.Event
	err   error
}

type xRead struct {
	window      xproto.Window
	owner       xproto.Window
	target      xproto.Atom
	time        xproto.Timestamp
	generation  uint64
	deadline    time.Time
	incremental bool
	valueType   xproto.Atom
	format      byte
	data        []byte
}

type xKey struct {
	window   xproto.Window
	property xproto.Atom
}

type xSend struct {
	data     []byte
	target   xproto.Atom
	deadline time.Time
}

type xPending struct {
	sequence   uint16
	generation uint64
	data       []byte
}

type x11 struct {
	b         *bridge
	conn      *xgb.Conn
	window    xproto.Window
	owner     xproto.Window
	atoms     map[string]xproto.Atom
	owned     bool
	data      []byte
	latin     []byte
	latinOK   bool
	ownedTime xproto.Timestamp
	read      *xRead
	pending   *xPending
	outgoing  map[xKey]*xSend
	chunkSize int
	events    chan xEvent
	done      chan struct{}
}

func openX11(b *bridge) (_ *x11, err error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	if err = xfixes.Init(conn); err != nil {
		return nil, err
	}
	if err = xcmisc.Init(conn); err != nil {
		return nil, err
	}
	conn.SetIDRangeFunc(func(c *xgb.Conn) (uint32, uint32, error) {
		rangeReply, err := xcmisc.GetXIDRange(c).Reply()
		if err != nil {
			return 0, 0, err
		}
		return rangeReply.StartId, rangeReply.Count, nil
	})
	version, err := xfixes.QueryVersion(conn, 5, 0).Reply()
	if err != nil {
		return nil, err
	}
	if version.MajorVersion < 1 {
		return nil, errors.New("XFixes selection notifications are unavailable")
	}
	x := &x11{b: b, conn: conn, atoms: make(map[string]xproto.Atom), outgoing: make(map[xKey]*xSend), events: make(chan xEvent, 32)}
	for _, name := range append([]string{"CLIPBOARD", "TARGETS", "TEXT", "STRING", "TIMESTAMP", "INCR", "_CLIP_SYNC_DATA", "_CLIP_SYNC_TIME"}, textMIMEs...) {
		atom, atomErr := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
		if atomErr != nil {
			return nil, atomErr
		}
		x.atoms[name] = atom.Atom
	}
	x.window, err = x.newWindow()
	if err != nil {
		return nil, err
	}
	mask := uint32(xfixes.SelectionEventMaskSetSelectionOwner | xfixes.SelectionEventMaskSelectionWindowDestroy | xfixes.SelectionEventMaskSelectionClientClose)
	if err = xfixes.SelectSelectionInputChecked(conn, x.window, x.atoms["CLIPBOARD"], mask).Check(); err != nil {
		return nil, err
	}
	owner, err := xproto.GetSelectionOwner(conn, x.atoms["CLIPBOARD"]).Reply()
	if err != nil {
		return nil, err
	}
	x.owner = owner.Owner
	x.chunkSize = min(64<<10, int(xproto.Setup(conn).MaximumRequestLength)*4-24)
	if x.chunkSize <= 0 {
		return nil, errors.New("invalid X11 request size limit")
	}
	return x, nil
}

func (x *x11) newWindow() (xproto.Window, error) {
	window, err := xproto.NewWindowId(x.conn)
	if err != nil {
		return 0, err
	}
	root := xproto.Setup(x.conn).DefaultScreen(x.conn).Root
	err = xproto.CreateWindowChecked(x.conn, 0, window, root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, xproto.CwEventMask, []uint32{xproto.EventMaskPropertyChange}).Check()
	return window, err
}

func (x *x11) start() {
	x.done = make(chan struct{})
	go func() {
		defer close(x.done)
		for {
			event, err := x.conn.WaitForEvent()
			var eventErr error = err
			if event == nil && err == nil {
				eventErr = io.EOF
			}
			select {
			case x.events <- xEvent{event: event, err: eventErr}:
			case <-x.b.ctx.Done():
				return
			}
			if eventErr != nil {
				return
			}
		}
	}()
}

func (x *x11) handle(event xgb.Event) error {
	switch e := event.(type) {
	case xfixes.SelectionNotifyEvent:
		if e.Selection != x.atoms["CLIPBOARD"] {
			return nil
		}
		x.owner = e.Owner
		if !x.b.ready || e.Owner == x.window {
			return nil
		}
		x.owned, x.data, x.latin = false, nil, nil
		if e.Owner == 0 {
			x.cancelRead()
			x.b.state.lost(x11Side)
			return nil
		}
		generation := x.b.changed(x11Side)
		window, err := x.newWindow()
		if err != nil {
			return err
		}
		x.read = &xRead{window: window, owner: e.Owner, time: e.Timestamp, generation: generation, deadline: time.Now().Add(transferTimeout)}
		if err := x.convert(x.atoms["TARGETS"]); err != nil {
			x.readFailed()
		}
	case xproto.SelectionNotifyEvent:
		r := x.read
		if r == nil || e.Requestor != r.window || e.Selection != x.atoms["CLIPBOARD"] || e.Target != r.target {
			return nil
		}
		if e.Property == 0 && r.target == x.atoms["TARGETS"] {
			if err := x.convert(x.atoms["UTF8_STRING"]); err != nil {
				x.readFailed()
			}
			return nil
		}
		if e.Property != x.atoms["_CLIP_SYNC_DATA"] {
			x.readFailed()
			return nil
		}
		if err := x.readProperty(); err != nil {
			x.readFailed()
		}
	case xproto.PropertyNotifyEvent:
		if e.Window == x.window && e.Atom == x.atoms["_CLIP_SYNC_TIME"] && e.State == xproto.PropertyNewValue {
			return x.claim(e)
		}
		if x.read != nil && x.read.incremental && e.Window == x.read.window && e.Atom == x.atoms["_CLIP_SYNC_DATA"] && e.State == xproto.PropertyNewValue {
			if err := x.readProperty(); err != nil {
				x.readFailed()
			}
		}
		if e.State == xproto.PropertyDelete {
			x.sendChunk(xKey{e.Window, e.Atom})
		}
	case xproto.SelectionRequestEvent:
		x.send(e)
	case xproto.SelectionClearEvent:
		if e.Selection == x.atoms["CLIPBOARD"] {
			x.owned, x.data, x.latin = false, nil, nil
		}
	case xproto.DestroyNotifyEvent:
		for key := range x.outgoing {
			if key.window == e.Window {
				x.finishSend(key)
			}
		}
	}
	return nil
}

func (x *x11) convert(target xproto.Atom) error {
	r := x.read
	r.target, r.data, r.valueType, r.format, r.incremental = target, nil, 0, 0, false
	return xproto.ConvertSelectionChecked(x.conn, r.window, x.atoms["CLIPBOARD"], target, x.atoms["_CLIP_SYNC_DATA"], r.time).Check()
}

func (x *x11) readProperty() error {
	r := x.read
	limit := maxClipboardBytes
	if r.target == x.atoms["TARGETS"] {
		limit = 4096
	}
	property, err := xproto.GetProperty(x.conn, true, r.window, x.atoms["_CLIP_SYNC_DATA"], xproto.GetPropertyTypeAny, 0, uint32((limit+3)/4)).Reply()
	if err != nil {
		return err
	}
	if property.BytesAfter != 0 {
		return errors.New("X11 selection exceeds size limit")
	}
	if !r.incremental && property.Type == x.atoms["INCR"] {
		if property.Format != 32 || len(property.Value) != 4 || xgb.Get32(property.Value) > uint32(limit) {
			return errors.New("invalid INCR size")
		}
		r.incremental = true
		return nil
	}
	if property.Type == 0 || (property.Format != 8 && property.Format != 32) {
		return errors.New("invalid X11 selection property")
	}
	if r.valueType == 0 {
		r.valueType, r.format = property.Type, property.Format
	}
	if property.Type != r.valueType || property.Format != r.format || len(r.data)+len(property.Value) > limit {
		return errors.New("invalid INCR chunk")
	}
	r.data = append(r.data, property.Value...)
	if r.incremental && len(property.Value) > 0 {
		return nil
	}
	if r.target == x.atoms["TARGETS"] {
		if r.valueType != xproto.AtomAtom || r.format != 32 || len(r.data)%4 != 0 {
			return errors.New("invalid X11 targets")
		}
		for _, name := range append(append([]string{}, textMIMEs...), "STRING", "TEXT") {
			for offset := 0; offset < len(r.data); offset += 4 {
				if xgb.Get32(r.data[offset:]) == uint32(x.atoms[name]) {
					return x.convert(x.atoms[name])
				}
			}
		}
		x.cancelRead()
		return nil
	}
	if r.format != 8 || !x.textTarget(r.valueType) {
		return errors.New("unsupported X11 text encoding")
	}
	data := r.data
	if r.valueType == x.atoms["STRING"] {
		data = fromLatin1(data)
	}
	if len(data) > maxClipboardBytes || !utf8.Valid(data) {
		return errors.New("invalid X11 text")
	}
	owner, err := xproto.GetSelectionOwner(x.conn, x.atoms["CLIPBOARD"]).Reply()
	if err != nil {
		return err
	}
	generation, expectedOwner := r.generation, r.owner
	x.cancelRead()
	if owner.Owner != expectedOwner {
		return nil
	}
	return x.b.received(x11Side, generation, data)
}

func (x *x11) textTarget(atom xproto.Atom) bool {
	if atom == x.atoms["STRING"] {
		return true
	}
	for _, mime := range textMIMEs {
		if atom == x.atoms[mime] {
			return true
		}
	}
	return false
}

func (x *x11) readFailed() {
	log.Print("clip-sync: ignored failed X11 text transfer")
	x.cancelRead()
}

func (x *x11) cancelRead() {
	if x.read != nil {
		window := x.read.window
		x.read = nil
		xproto.DestroyWindowChecked(x.conn, window).Check()
	}
}

func (x *x11) publish(generation uint64, data []byte) error {
	// A server timestamp prevents claiming over a later X11 owner change.
	cookie := xproto.ChangePropertyChecked(x.conn, xproto.PropModeReplace, x.window, x.atoms["_CLIP_SYNC_TIME"], xproto.AtomInteger, 8, 1, []byte{0})
	x.pending = &xPending{sequence: cookie.Sequence, generation: generation, data: data}
	return cookie.Check()
}

func (x *x11) claim(event xproto.PropertyNotifyEvent) error {
	pending := x.pending
	if pending == nil || pending.sequence != event.Sequence {
		return nil
	}
	x.pending = nil
	if !x.b.state.current(pending.generation) {
		return nil
	}
	owner, err := xproto.GetSelectionOwner(x.conn, x.atoms["CLIPBOARD"]).Reply()
	if err != nil {
		return err
	}
	if owner.Owner != x.owner {
		return nil
	}
	if err := xproto.SetSelectionOwnerChecked(x.conn, x.window, x.atoms["CLIPBOARD"], event.Time).Check(); err != nil {
		return err
	}
	owner, err = xproto.GetSelectionOwner(x.conn, x.atoms["CLIPBOARD"]).Reply()
	if err != nil {
		return err
	}
	if owner.Owner != x.window {
		return nil
	}
	x.owned, x.owner, x.data, x.ownedTime = true, x.window, pending.data, event.Time
	x.latin, x.latinOK = toLatin1(pending.data)
	x.b.state.copied(x11Side, pending.data)
	return nil
}

func (x *x11) send(e xproto.SelectionRequestEvent) {
	property := e.Property
	if property == 0 {
		property = e.Target
	}
	notify := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target}
	defer func() {
		xproto.SendEventChecked(x.conn, false, e.Requestor, 0, string(notify.Bytes())).Check()
	}()
	if !x.owned || e.Selection != x.atoms["CLIPBOARD"] || e.Owner != x.window || !validXTime(e.Time, x.ownedTime) {
		return
	}
	var data []byte
	target, format := e.Target, byte(8)
	switch e.Target {
	case x.atoms["TARGETS"]:
		target, format = xproto.AtomAtom, 32
		names := append([]string{"TARGETS", "TIMESTAMP", "TEXT"}, textMIMEs...)
		if x.latinOK {
			names = append(names, "STRING")
		}
		data = make([]byte, 4*len(names))
		for i, name := range names {
			xgb.Put32(data[i*4:], uint32(x.atoms[name]))
		}
	case x.atoms["TIMESTAMP"]:
		target, format, data = xproto.AtomInteger, 32, make([]byte, 4)
		xgb.Put32(data, uint32(x.ownedTime))
	case x.atoms["TEXT"]:
		target, data = x.atoms["UTF8_STRING"], x.data
	case x.atoms["STRING"]:
		if !x.latinOK {
			return
		}
		data = x.latin
	default:
		if !x.textTarget(e.Target) {
			return
		}
		data = x.data
	}
	key := xKey{e.Requestor, property}
	if _, exists := x.outgoing[key]; exists {
		return
	}
	if len(data) <= x.chunkSize {
		if x.changeProperty(key, target, format, data) == nil {
			notify.Property = property
		}
		return
	}
	if len(x.outgoing) >= maxTransfers {
		return
	}
	mask := uint32(xproto.EventMaskPropertyChange | xproto.EventMaskStructureNotify)
	if xproto.ChangeWindowAttributesChecked(x.conn, e.Requestor, xproto.CwEventMask, []uint32{mask}).Check() != nil {
		return
	}
	x.outgoing[key] = &xSend{data: data, target: target, deadline: time.Now().Add(transferTimeout)}
	size := make([]byte, 4)
	xgb.Put32(size, uint32(len(data)))
	if x.changeProperty(key, x.atoms["INCR"], 32, size) != nil {
		x.finishSend(key)
		return
	}
	notify.Property = property
}

func validXTime(request, owned xproto.Timestamp) bool {
	// Subtraction preserves ordering across the 32-bit timestamp wrap.
	return request == xproto.TimeCurrentTime || int32(request-owned) >= 0
}

func (x *x11) changeProperty(key xKey, target xproto.Atom, format byte, data []byte) error {
	return xproto.ChangePropertyChecked(x.conn, xproto.PropModeReplace, key.window, key.property, target, format, uint32(len(data)/(int(format)/8)), data).Check()
}

func (x *x11) sendChunk(key xKey) {
	transfer := x.outgoing[key]
	if transfer == nil {
		return
	}
	n := min(len(transfer.data), x.chunkSize)
	if x.changeProperty(key, transfer.target, 8, transfer.data[:n]) != nil || n == 0 {
		x.finishSend(key)
		return
	}
	transfer.data = transfer.data[n:]
}

func (x *x11) finishSend(key xKey) {
	delete(x.outgoing, key)
	for other := range x.outgoing {
		if other.window == key.window {
			return
		}
	}
	xproto.ChangeWindowAttributesChecked(x.conn, key.window, xproto.CwEventMask, []uint32{0}).Check()
}

func (x *x11) expire(now time.Time) {
	if x.read != nil && !now.Before(x.read.deadline) {
		x.readFailed()
	}
	for key, transfer := range x.outgoing {
		if !now.Before(transfer.deadline) {
			x.finishSend(key)
		}
	}
}

func (x *x11) close() {
	x.conn.Close()
	if x.done != nil {
		<-x.done
	}
}
