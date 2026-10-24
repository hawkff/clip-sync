package main

import (
	"errors"
	"io"
	"log"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xcmisc"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// x11TextTargets lists X11 text targets in order of preference.
var x11TextTargets = append(slices.Clone(textMIMEs), "STRING", "TEXT")

type xEvent struct {
	event xgb.Event
	err   error
}

type xRead struct {
	window      xproto.Window
	owner       xproto.Window
	time        xproto.Timestamp
	generation  uint64
	deadline    time.Time // for the whole read
	turn        time.Time // for the current target
	targets     []string  // formats to convert after target
	target      xproto.Atom
	incremental bool
	valueType   xproto.Atom
	format      byte
	data        []byte
	clip        clip
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
	data       clip
}

// xSelection tracks one X11 selection: CLIPBOARD or PRIMARY.
type xSelection struct {
	atom      xproto.Atom
	owner     xproto.Window
	owned     bool
	data      clip
	latin     []byte
	latinOK   bool
	ownedTime xproto.Timestamp
	read      *xRead
	pending   *xPending
	// unsetTime is the server time of our last unset, to recognize its notification.
	unsetTime xproto.Timestamp
	unsetting bool
}

type x11 struct {
	b         *bridge
	conn      *xgb.Conn
	window    xproto.Window
	atoms     map[string]xproto.Atom
	names     map[xproto.Atom]string
	sel       [2]xSelection
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
	x := &x11{b: b, conn: conn, atoms: make(map[string]xproto.Atom), names: make(map[xproto.Atom]string), outgoing: make(map[xKey]*xSend), events: make(chan xEvent, 32)}
	names := []string{"CLIPBOARD", "PRIMARY", "TARGETS", "TEXT", "STRING", "TIMESTAMP", "INCR", "_CLIP_SYNC_DATA", "_CLIP_SYNC_TIME"}
	for _, list := range [][]string{textMIMEs, extraMIMEs, imageMIMEs} {
		names = append(names, list...)
	}
	for _, name := range names {
		atom, atomErr := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
		if atomErr != nil {
			return nil, atomErr
		}
		x.atoms[name] = atom.Atom
		x.names[atom.Atom] = name
	}
	x.window, err = x.newWindow()
	if err != nil {
		return nil, err
	}
	mask := uint32(xfixes.SelectionEventMaskSetSelectionOwner | xfixes.SelectionEventMaskSelectionWindowDestroy | xfixes.SelectionEventMaskSelectionClientClose)
	for i, name := range []string{"CLIPBOARD", "PRIMARY"} {
		s := &x.sel[i]
		s.atom = x.atoms[name]
		if err = xfixes.SelectSelectionInputChecked(conn, x.window, s.atom, mask).Check(); err != nil {
			return nil, err
		}
		owner, ownerErr := xproto.GetSelectionOwner(conn, s.atom).Reply()
		if ownerErr != nil {
			return nil, ownerErr
		}
		s.owner = owner.Owner
	}
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

func (x *x11) selectionFor(atom xproto.Atom) (selection, bool) {
	for i := range x.sel {
		if x.sel[i].atom == atom {
			return selection(i), true
		}
	}
	return 0, false
}

func (x *x11) readFor(window xproto.Window) (selection, bool) {
	for i := range x.sel {
		if r := x.sel[i].read; r != nil && r.window == window {
			return selection(i), true
		}
	}
	return 0, false
}

func (x *x11) handle(event xgb.Event) error {
	switch e := event.(type) {
	case xfixes.SelectionNotifyEvent:
		sel, ok := x.selectionFor(e.Selection)
		if !ok {
			return nil
		}
		s := &x.sel[sel]
		s.owner = e.Owner
		if !x.b.ready || !x.b.active(sel) || e.Owner == x.window {
			return nil
		}
		if e.Owner == 0 && s.unsetting && e.Subtype == xfixes.SelectionEventSetSelectionOwner && e.SelectionTimestamp == s.unsetTime {
			s.unsetting = false
			return nil
		}
		s.owned, s.data, s.latin = false, nil, nil
		x.cancelRead(sel)
		if e.Owner == 0 {
			return x.b.emptied(sel, x11Side)
		}
		generation := x.b.changed(sel, x11Side)
		window, err := x.newWindow()
		if err != nil {
			return err
		}
		s.read = &xRead{window: window, owner: e.Owner, time: e.Timestamp, generation: generation, deadline: time.Now().Add(transferTimeout), clip: clip{}}
		if x.convert(sel, x.atoms["TARGETS"]) != nil {
			x.readFailed(sel)
		}
	case xproto.SelectionNotifyEvent:
		sel, ok := x.readFor(e.Requestor)
		if !ok {
			return nil
		}
		r := x.sel[sel].read
		if e.Selection != x.sel[sel].atom || e.Target != r.target {
			return nil
		}
		if e.Property == 0 && r.target == x.atoms["TARGETS"] {
			// Some owners refuse TARGETS but still convert UTF8_STRING.
			r.targets = []string{"UTF8_STRING"}
			return x.nextTarget(sel)
		}
		if e.Property != x.atoms["_CLIP_SYNC_DATA"] {
			return x.targetFailed(sel)
		}
		return x.progress(sel)
	case xproto.PropertyNotifyEvent:
		if e.Window == x.window && e.Atom == x.atoms["_CLIP_SYNC_TIME"] && e.State == xproto.PropertyNewValue {
			return x.claim(e)
		}
		if sel, ok := x.readFor(e.Window); ok && x.sel[sel].read.incremental && e.Atom == x.atoms["_CLIP_SYNC_DATA"] && e.State == xproto.PropertyNewValue {
			return x.progress(sel)
		}
		if e.State == xproto.PropertyDelete {
			x.sendChunk(xKey{e.Window, e.Atom})
		}
	case xproto.SelectionRequestEvent:
		x.send(e)
	case xproto.SelectionClearEvent:
		if sel, ok := x.selectionFor(e.Selection); ok {
			s := &x.sel[sel]
			s.owned, s.data, s.latin = false, nil, nil
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

func (x *x11) convert(sel selection, target xproto.Atom) error {
	r := x.sel[sel].read
	r.target, r.data, r.valueType, r.format, r.incremental = target, nil, 0, 0, false
	// Each target gets a share of the time left, so one stalled format cannot starve the rest.
	r.turn = time.Now().Add(time.Until(r.deadline) / time.Duration(1+len(r.targets)))
	return xproto.ConvertSelectionChecked(x.conn, r.window, x.sel[sel].atom, target, x.atoms["_CLIP_SYNC_DATA"], r.time).Check()
}

// progress reads the next chunk of the current target and finishes the target
// once all of its data has arrived.
func (x *x11) progress(sel selection) error {
	done, err := x.readProperty(sel)
	if err != nil {
		return x.targetFailed(sel)
	}
	if !done {
		return nil
	}
	return x.finishTarget(sel)
}

func (x *x11) readProperty(sel selection) (bool, error) {
	r := x.sel[sel].read
	limit := maxClipboardBytes
	if r.target == x.atoms["TARGETS"] {
		limit = 4096
	}
	property, err := xproto.GetProperty(x.conn, true, r.window, x.atoms["_CLIP_SYNC_DATA"], xproto.GetPropertyTypeAny, 0, uint32((limit+3)/4)).Reply()
	if err != nil {
		return false, err
	}
	if property.BytesAfter != 0 {
		return false, errors.New("X11 selection exceeds size limit")
	}
	if !r.incremental && property.Type == x.atoms["INCR"] {
		// The value is a lower bound on the size. Some owners, such as xclip, omit it.
		if property.Format != 32 || len(property.Value) > 4 || len(property.Value) == 4 && xgb.Get32(property.Value) > uint32(limit) {
			return false, errors.New("invalid INCR size")
		}
		r.incremental = true
		return false, nil
	}
	if property.Type == 0 || (property.Format != 8 && property.Format != 32) {
		return false, errors.New("invalid X11 selection property")
	}
	if r.valueType == 0 {
		r.valueType, r.format = property.Type, property.Format
	}
	if property.Type != r.valueType || property.Format != r.format || len(r.data)+len(property.Value) > limit {
		return false, errors.New("invalid INCR chunk")
	}
	r.data = append(r.data, property.Value...)
	return !r.incremental || len(property.Value) == 0, nil
}

func (x *x11) finishTarget(sel selection) error {
	r := x.sel[sel].read
	if r.target == x.atoms["TARGETS"] {
		if r.valueType != xproto.AtomAtom || r.format != 32 || len(r.data)%4 != 0 {
			return x.targetFailed(sel)
		}
		offered := make(map[string]bool)
		for offset := 0; offset < len(r.data); offset += 4 {
			if name, ok := x.names[xproto.Atom(xgb.Get32(r.data[offset:]))]; ok {
				offered[name] = true
			}
		}
		r.targets = wanted(offered, x11TextTargets)
		if len(r.targets) == 0 {
			// Nothing to copy; the other clipboard keeps its content.
			x.b.state[sel].failed(r.generation)
			x.cancelRead(sel)
			return nil
		}
		return x.nextTarget(sel)
	}
	if r.format != 8 {
		return x.targetFailed(sel)
	}
	name, data := x.names[r.target], r.data
	if slices.Contains(x11TextTargets, name) {
		if r.valueType == x.atoms["STRING"] {
			data = fromLatin1(data)
		} else if !slices.Contains(textMIMEs, x.names[r.valueType]) {
			return x.targetFailed(sel)
		}
		if len(data) > maxClipboardBytes || !utf8.Valid(data) {
			return x.targetFailed(sel)
		}
		name = textKey
	}
	r.clip[name] = data
	return x.nextTarget(sel)
}

func (x *x11) nextTarget(sel selection) error {
	r := x.sel[sel].read
	for len(r.targets) > 0 {
		target := x.atoms[r.targets[0]]
		r.targets = r.targets[1:]
		if x.convert(sel, target) == nil {
			return nil
		}
	}
	return x.finishRead(sel)
}

// targetFailed skips the current target. Later targets use a new window
// because the owner may still write to the old one.
func (x *x11) targetFailed(sel selection) error {
	r := x.sel[sel].read
	if r.target == x.atoms["TARGETS"] {
		x.readFailed(sel)
		return nil
	}
	window, err := x.newWindow()
	if err != nil {
		return err
	}
	xproto.DestroyWindowChecked(x.conn, r.window).Check()
	r.window = window
	return x.nextTarget(sel)
}

func (x *x11) finishRead(sel selection) error {
	s := &x.sel[sel]
	r := s.read
	if len(r.clip) == 0 {
		x.readFailed(sel)
		return nil
	}
	owner, err := xproto.GetSelectionOwner(x.conn, s.atom).Reply()
	if err != nil {
		return err
	}
	generation, expectedOwner, data := r.generation, r.owner, r.clip
	x.cancelRead(sel)
	if owner.Owner != expectedOwner {
		return nil
	}
	return x.b.received(sel, x11Side, generation, data)
}

func (x *x11) readFailed(sel selection) {
	log.Print("clip-sync: ignored failed X11 transfer")
	if r := x.sel[sel].read; r != nil {
		x.b.state[sel].failed(r.generation)
	}
	x.cancelRead(sel)
}

func (x *x11) cancelRead(sel selection) {
	s := &x.sel[sel]
	if s.read != nil {
		window := s.read.window
		s.read = nil
		xproto.DestroyWindowChecked(x.conn, window).Check()
	}
}

func (x *x11) publish(sel selection, generation uint64, data clip) error {
	// A server timestamp prevents claiming over a later X11 owner change.
	cookie := xproto.ChangePropertyChecked(x.conn, xproto.PropModeReplace, x.window, x.atoms["_CLIP_SYNC_TIME"], xproto.AtomInteger, 8, 1, []byte{0})
	x.sel[sel].pending = &xPending{sequence: cookie.Sequence, generation: generation, data: data}
	return cookie.Check()
}

func (x *x11) claim(event xproto.PropertyNotifyEvent) error {
	for i := range x.sel {
		sel, s := selection(i), &x.sel[i]
		pending := s.pending
		if pending == nil || pending.sequence != event.Sequence {
			continue
		}
		s.pending = nil
		if !x.b.state[sel].current(pending.generation) {
			return nil
		}
		owner, err := xproto.GetSelectionOwner(x.conn, s.atom).Reply()
		if err != nil {
			return err
		}
		if owner.Owner != s.owner {
			return nil
		}
		if len(pending.data) == 0 {
			return x.unset(sel, event.Time)
		}
		if err := xproto.SetSelectionOwnerChecked(x.conn, x.window, s.atom, event.Time).Check(); err != nil {
			return err
		}
		owner, err = xproto.GetSelectionOwner(x.conn, s.atom).Reply()
		if err != nil {
			return err
		}
		if owner.Owner != x.window {
			return nil
		}
		s.owned, s.owner, s.data, s.ownedTime = true, x.window, pending.data, event.Time
		s.latin, s.latinOK = nil, false
		if text, ok := pending.data[textKey]; ok {
			s.latin, s.latinOK = toLatin1(text)
		}
		x.b.state[sel].copied(x11Side, pending.data)
		return nil
	}
	return nil
}

// unset clears an X11 selection to mirror an empty Wayland clipboard.
func (x *x11) unset(sel selection, time xproto.Timestamp) error {
	s := &x.sel[sel]
	if s.owner != 0 {
		if err := xproto.SetSelectionOwnerChecked(x.conn, 0, s.atom, time).Check(); err != nil {
			return err
		}
		owner, err := xproto.GetSelectionOwner(x.conn, s.atom).Reply()
		if err != nil {
			return err
		}
		if owner.Owner != 0 {
			return nil
		}
		s.unsetTime, s.unsetting = time, true
	}
	s.owned, s.data, s.latin, s.latinOK = false, nil, nil, false
	x.b.state[sel].copied(x11Side, nil)
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
	sel, ok := x.selectionFor(e.Selection)
	if !ok {
		return
	}
	s := &x.sel[sel]
	if !s.owned || e.Owner != x.window || !validXTime(e.Time, s.ownedTime) {
		return
	}
	text, hasText := s.data[textKey]
	name := x.names[e.Target]
	var data []byte
	target, format := e.Target, byte(8)
	switch {
	case name == "TARGETS":
		target, format = xproto.AtomAtom, 32
		names := []string{"TARGETS", "TIMESTAMP"}
		if hasText {
			names = append(append(names, "TEXT"), textMIMEs...)
			if s.latinOK {
				names = append(names, "STRING")
			}
		}
		names = append(names, s.data.formats()...)
		data = make([]byte, 4*len(names))
		for i, offered := range names {
			xgb.Put32(data[i*4:], uint32(x.atoms[offered]))
		}
	case name == "TIMESTAMP":
		target, format, data = xproto.AtomInteger, 32, make([]byte, 4)
		xgb.Put32(data, uint32(s.ownedTime))
	case slices.Contains(x11TextTargets, name):
		switch {
		case !hasText:
			return
		case name == "TEXT":
			target, data = x.atoms["UTF8_STRING"], text
		case name == "STRING":
			if !s.latinOK {
				return
			}
			data = s.latin
		default:
			data = text
		}
	default:
		value, ok := s.data[name]
		if !ok {
			return
		}
		data = value
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

func (x *x11) expire(now time.Time) error {
	for i := range x.sel {
		r := x.sel[i].read
		switch {
		case r == nil:
		case !now.Before(r.deadline):
			// The read ran out of time: keep the formats already read.
			r.targets = nil
			if err := x.finishRead(selection(i)); err != nil {
				return err
			}
		case !now.Before(r.turn):
			if err := x.targetFailed(selection(i)); err != nil {
				return err
			}
		}
	}
	for key, transfer := range x.outgoing {
		if !now.Before(transfer.deadline) {
			x.finishSend(key)
		}
	}
	return nil
}

func (x *x11) close() {
	x.conn.Close()
	if x.done != nil {
		<-x.done
	}
}
