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
	maxTransfers      = 8
)

type side uint8

const (
	waylandSide side = iota
	x11Side
)

type selection uint8

const (
	selClipboard selection = iota
	selPrimary
)

// textKey is where a clip keeps its text, always as UTF-8.
const textKey = "text/plain;charset=utf-8"

// textMIMEs lists UTF-8 text types in order of preference.
var textMIMEs = []string{textKey, "text/plain;charset=UTF-8", "UTF8_STRING", "text/plain"}

// extraMIMEs are rich-text and file-list types, copied byte for byte.
var extraMIMEs = []string{"text/html", "text/rtf", "text/uri-list", "x-special/gnome-copied-files", "application/x-kde-cutselection"}

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

// syncState orders the copies of one selection across both sides.
type syncState struct {
	generation uint64
	origin     side
	reading    bool
	clipboard  [2]snapshot
}

// changed starts a generation for a new copy on side from.
func (s *syncState) changed(from side) uint64 {
	s.generation++
	s.origin = from
	s.reading = true
	s.clipboard[from] = snapshot{}
	return s.generation
}

func (s *syncState) current(generation uint64) bool {
	return generation == s.generation
}

// received records the copy read from side from and reports whether the
// other side needs it.
func (s *syncState) received(from side, generation uint64, data clip) bool {
	if !s.current(generation) {
		return false
	}
	s.reading = false
	s.clipboard[from] = snapshot{known: true, data: data}
	other := s.clipboard[1-from]
	return !other.known || other.opaque || !data.equal(other.data)
}

// failed records that the copy for generation could not be read.
func (s *syncState) failed(generation uint64) {
	if s.current(generation) {
		s.reading = false
		s.clipboard[s.origin] = snapshot{known: true, opaque: true}
	}
}

// emptied records that side from lost its owner. It reports whether the other
// side must be cleared too, and the generation for that clear. The other side
// is cleared when it held the same content, or when the latest copy came from
// this side and may still be on its way there. A newer copy from the other
// side survives.
func (s *syncState) emptied(from side) (uint64, bool) {
	before, other := s.clipboard[from], s.clipboard[1-from]
	s.clipboard[from] = snapshot{known: true}
	if s.reading && s.origin != from {
		// A newer copy from the other side is still being read.
		return 0, false
	}
	read := before.known && !before.opaque
	synced := read && other.known && !other.opaque && before.data.equal(other.data)
	otherEmpty := other.known && !other.opaque && len(other.data) == 0
	mirror := (synced || read && s.origin == from) && !otherEmpty
	if !mirror && !s.reading {
		// Keep the generation, so a copy on its way to this side still arrives.
		return 0, false
	}
	// A clear supersedes pending copies, and a read from the emptied side is stale.
	generation := s.changed(from)
	s.reading = false
	s.clipboard[from] = snapshot{known: true}
	return generation, mirror
}

func (s *syncState) copied(to side, data clip) {
	s.clipboard[to] = snapshot{known: true, data: data}
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
