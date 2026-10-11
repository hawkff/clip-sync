package main

import (
	"bytes"
	"errors"
	"testing"
)

func TestStaleReadCannotOverwriteNewCopy(t *testing.T) {
	for _, first := range []side{waylandSide, x11Side} {
		var s syncState
		old := s.changed(first)
		fresh := s.changed(1 - first)
		if !s.received(1-first, fresh, []byte("fresh")) {
			t.Fatal("fresh copy was dropped")
		}
		s.copied(first, []byte("fresh"))
		if s.received(first, old, []byte("old")) {
			t.Fatal("stale transfer would overwrite fresh text")
		}
	}
}

func TestContentEchoAndRepeatedCopy(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if !s.received(waylandSide, generation, []byte("A")) {
		t.Fatal("initial copy was dropped")
	}
	s.copied(x11Side, []byte("A"))
	generation = s.changed(x11Side)
	if s.received(x11Side, generation, []byte("A")) {
		t.Fatal("echo would bounce back")
	}
	generation = s.changed(x11Side)
	if !s.received(x11Side, generation, []byte("B")) {
		t.Fatal("new content was dropped")
	}
	s.copied(waylandSide, []byte("B"))
	generation = s.changed(waylandSide)
	if !s.received(waylandSide, generation, []byte("A")) {
		t.Fatal("copying earlier content again was suppressed")
	}
}

func TestOwnershipLossIsNotEmptyText(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	s.lost(x11Side)
	if !s.received(waylandSide, generation, []byte("fresh")) {
		t.Fatal("unrelated owner loss discarded the fresh copy")
	}
	s.copied(x11Side, []byte("fresh"))
	generation = s.changed(waylandSide)
	s.lost(waylandSide)
	if s.received(waylandSide, generation, []byte("stale")) {
		t.Fatal("a vanished owner completed a stale transfer")
	}
	generation = s.changed(waylandSide)
	if !s.received(waylandSide, generation, []byte{}) {
		t.Fatal("explicit empty text was suppressed")
	}
}

func TestFailedWriteDoesNotAcknowledgeDestination(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if !s.received(waylandSide, generation, []byte("text")) {
		t.Fatal("initial copy was dropped")
	}
	generation = s.changed(waylandSide)
	if !s.received(waylandSide, generation, []byte("text")) {
		t.Fatal("failed write was treated as a successful copy")
	}
}

func TestReadText(t *testing.T) {
	for _, input := range [][]byte{nil, []byte("line one\r\nline two\n"), []byte("こんにちは 🌍"), bytes.Repeat([]byte("x"), 1024)} {
		got, err := readText(bytes.NewReader(input), 1024)
		if err != nil || !bytes.Equal(got, input) {
			t.Fatalf("text changed: length=%d error=%v", len(input), err)
		}
	}
	for _, input := range [][]byte{{0xff}, bytes.Repeat([]byte("x"), 1025)} {
		if _, err := readText(bytes.NewReader(input), 1024); err == nil {
			t.Fatal("invalid input was accepted")
		}
	}
	if data, err := readText(failedReader{}, 1024); err == nil || data != nil {
		t.Fatal("read failure was converted to clipboard data")
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestLatin1(t *testing.T) {
	input := []byte{0x00, 0x41, 0x80, 0xe9, 0xff}
	encoded, ok := toLatin1(fromLatin1(input))
	if !ok || !bytes.Equal(input, encoded) {
		t.Fatal("Latin-1 roundtrip changed text")
	}
	if _, ok := toLatin1([]byte("🌍")); ok {
		t.Fatal("unrepresentable text was accepted as Latin-1")
	}
}
