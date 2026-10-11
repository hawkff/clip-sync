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

type snapshot struct {
	known bool
	data  []byte
}

type syncState struct {
	generation uint64
	origin     side
	clipboard  [2]snapshot
}

func (s *syncState) changed(from side) uint64 {
	s.generation++
	s.origin = from
	s.clipboard[from] = snapshot{}
	return s.generation
}

func (s *syncState) lost(from side) {
	s.clipboard[from] = snapshot{}
	if s.origin == from {
		s.generation++
	}
}

func (s *syncState) current(generation uint64) bool {
	return generation == s.generation
}

func (s *syncState) received(from side, generation uint64, data []byte) bool {
	if !s.current(generation) {
		return false
	}
	s.clipboard[from] = snapshot{known: true, data: data}
	other := s.clipboard[1-from]
	return !other.known || !bytes.Equal(data, other.data)
}

func (s *syncState) copied(to side, data []byte) {
	s.clipboard[to] = snapshot{known: true, data: data}
}

func readText(r io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("clipboard exceeds size limit")
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
