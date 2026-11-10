package main

import (
	"bytes"
	"errors"
	"io"
	"time"
	"unicode/utf8"
)

const (
	maxClipboardBytes = 16 << 20
	transferTimeout   = 10 * time.Second
	// verifyTimeout bounds a read made only to compare two sides, so a side
	// that does not answer cannot hold up the newer copy for long.
	// shortcut: the whole read gets this time, so a mirrored copy too large to
	// read in it still loses its owner; switch to a deadline per chunk if that shows up.
	verifyTimeout = 500 * time.Millisecond
	// settleDelay is how long an X11 copy waits before it goes to Wayland. A
	// compositor that bridges X11 itself shows its own copy within that time.
	settleDelay = 50 * time.Millisecond
	// One clip offers up to 12 Wayland types, and a clipboard manager may ask
	// for all of them at once.
	maxTransfers = 16
)

type side uint8

const (
	waylandSide side = iota
	x11Side
)

func (s side) String() string {
	if s == x11Side {
		return "X11"
	}
	return "Wayland"
}

type selection uint8

const (
	selClipboard selection = iota
	selPrimary
)

// textKey is where a clip keeps its text, always as UTF-8.
const textKey = "text/plain;charset=utf-8"

// textMIMEs lists UTF-8 text types in order of preference.
var textMIMEs = []string{textKey, "text/plain;charset=UTF-8", "UTF8_STRING", "text/plain"}

// extraMIMEs are rich-text and file-list types, and the mark a password
// manager puts on a secret so clipboard managers skip it. All are copied byte
// for byte.
var extraMIMEs = []string{"text/html", "text/rtf", "text/uri-list", "x-special/gnome-copied-files", "application/x-kde-cutselection", "x-kde-passwordManagerHint"}

// imageMIMEs lists image types in order of preference. Only the first one offered is copied.
var imageMIMEs = []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp"}

// clip holds the formats of one clipboard: text under textKey, other formats
// under their MIME types. An empty clip stands for an empty clipboard.
type clip map[string][]byte

func (c clip) equal(other clip) bool {
	if len(c) != len(other) {
		return false
	}
	for mime, data := range c {
		value, ok := other[mime]
		if !ok || !bytes.Equal(data, value) {
			return false
		}
	}
	return true
}

// formats lists the non-text formats in c in a stable order.
func (c clip) formats() []string {
	var mimes []string
	for _, list := range [][]string{extraMIMEs, imageMIMEs} {
		for _, mime := range list {
			if _, ok := c[mime]; ok {
				mimes = append(mimes, mime)
			}
		}
	}
	return mimes
}

// wanted picks the offered formats to copy: the first offered type from
// textTypes, every rich-text and file-list type, and the first image type.
func wanted(offered map[string]bool, textTypes []string) []string {
	var mimes []string
	if mime, ok := firstOffered(offered, textTypes); ok {
		mimes = append(mimes, mime)
	}
	for _, mime := range extraMIMEs {
		if offered[mime] {
			mimes = append(mimes, mime)
		}
	}
	if mime, ok := firstOffered(offered, imageMIMEs); ok {
		mimes = append(mimes, mime)
	}
	return mimes
}

func firstOffered(offered map[string]bool, mimes []string) (string, bool) {
	for _, mime := range mimes {
		if offered[mime] {
			return mime, true
		}
	}
	return "", false
}

type snapshot struct {
	known  bool // observed since the clipboard last changed
	opaque bool // holds content the bridge could not copy
	data   clip
}

// stepKind is what the bridge does after a read or a clear.
type stepKind uint8

const (
	stepNone stepKind = iota
	// stepPublish copies data to side to. No data clears that side.
	stepPublish
	// stepVerify reads side to for comparison, before a newer copy replaces it.
	stepVerify
	// stepReread reads side to as a new copy.
	stepReread
)

type step struct {
	kind stepKind
	to   side
	data clip
}

// syncState orders the copies of one selection across both sides.
//
// A compositor that bridges X11 itself puts each copy on both sides, so both
// change together. The bridge then reads both and leaves them alone when they
// match, instead of taking either selection from its owner.
type syncState struct {
	generation uint64
	origin     side
	reading    bool
	// verifying is set while the read in flight checks the side that a newer
	// copy is about to replace.
	verifying bool
	// unsent marks a side whose own copy has not reached the other side.
	unsent    [2]bool
	clipboard [2]snapshot
}

// changed starts a generation for a new copy on side from.
func (s *syncState) changed(from side) uint64 {
	s.generation++
	s.origin = from
	s.reading, s.verifying = true, false
	s.unsent[from] = true
	s.clipboard[from] = snapshot{}
	return s.generation
}

func (s *syncState) current(generation uint64) bool {
	return generation == s.generation
}

// received records the copy read from side from and reports the next step.
func (s *syncState) received(from side, generation uint64, data clip) step {
	if !s.current(generation) {
		return step{}
	}
	s.reading = false
	s.clipboard[from] = snapshot{known: true, data: data}
	other := s.clipboard[1-from]
	if s.verifying {
		// The other side holds the newer copy, and data is what it would replace.
		s.verifying = false
		s.unsent[from] = false
		if data.equal(other.data) {
			s.unsent[1-from] = false
			return step{}
		}
		return step{stepPublish, from, other.data}
	}
	switch {
	case s.unsent[1-from] && !other.known:
		// The other side changed too and was never read.
		s.reading, s.verifying = true, true
		return step{stepVerify, 1 - from, nil}
	case !other.known || other.opaque || !data.equal(other.data):
		// This copy replaces whatever the other side still had to send.
		s.unsent[1-from] = false
		return step{stepPublish, 1 - from, data}
	}
	s.unsent = [2]bool{}
	return step{}
}

// failed records that side from holds nothing the bridge can copy for
// generation, and reports the next step. The other clipboard keeps its
// content, and a copy of its own that never crossed replaces what failed.
func (s *syncState) failed(from side, generation uint64) step {
	if !s.current(generation) {
		return step{}
	}
	s.reading = false
	s.clipboard[from] = snapshot{known: true, opaque: true}
	s.unsent[from] = false
	other := s.clipboard[1-from]
	switch {
	case s.verifying:
		s.verifying = false
		return step{stepPublish, from, other.data}
	case s.unsent[1-from] && other.known:
		s.origin = 1 - from
		return step{stepPublish, from, other.data}
	case s.unsent[1-from]:
		return step{stepReread, 1 - from, nil}
	}
	return step{}
}

// emptied records that side from lost its owner and reports the next step.
// The other side is cleared too when it held the same content, or when the
// latest copy came from this side and may still be on its way there. A copy
// of its own that the other side still holds survives and syncs instead.
func (s *syncState) emptied(from side) step {
	before, other := s.clipboard[from], s.clipboard[1-from]
	pending := s.unsent[from]
	s.clipboard[from] = snapshot{known: true}
	s.unsent[from] = false
	if s.reading && s.origin != from && !s.verifying {
		// A newer copy from the other side is still being read.
		return step{}
	}
	if s.unsent[1-from] {
		s.reading, s.verifying = false, false
		if !other.known {
			return step{stepReread, 1 - from, nil}
		}
		// A read from the emptied side is stale.
		s.generation++
		s.origin = 1 - from
		return step{stepPublish, from, other.data}
	}
	read := before.known && !before.opaque
	synced := read && other.known && !other.opaque && before.data.equal(other.data)
	otherEmpty := other.known && !other.opaque && len(other.data) == 0
	mirror := (synced || read && s.origin == from) && !otherEmpty
	if !mirror && !s.reading && !pending {
		return step{}
	}
	// A clear supersedes pending copies, and a read from the emptied side is stale.
	s.changed(from)
	s.reading, s.unsent[from] = false, false
	s.clipboard[from] = snapshot{known: true}
	if !mirror {
		return step{}
	}
	return step{stepPublish, 1 - from, nil}
}

// copied records that side to now holds data, the same content as the other side.
func (s *syncState) copied(to side, data clip) {
	s.clipboard[to] = snapshot{known: true, data: data}
	s.unsent = [2]bool{}
}

func readData(r io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("clipboard exceeds size limit")
	}
	return data, nil
}

func readText(r io.Reader, limit int) ([]byte, error) {
	data, err := readData(r, limit)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("clipboard is not UTF-8 text")
	}
	return data, nil
}

func fromLatin1(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		out = utf8.AppendRune(out, rune(b))
	}
	return out
}

func toLatin1(data []byte) ([]byte, bool) {
	out := make([]byte, 0, len(data))
	for _, r := range string(data) {
		if r > 255 {
			return nil, false
		}
		out = append(out, byte(r))
	}
	return out, true
}
