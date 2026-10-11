package main

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

func text(s string) clip {
	return clip{textKey: []byte(s)}
}

func TestStaleReadCannotOverwriteNewCopy(t *testing.T) {
	for _, first := range []side{waylandSide, x11Side} {
		var s syncState
		old := s.changed(first)
		fresh := s.changed(1 - first)
		if !s.received(1-first, fresh, text("fresh")) {
			t.Fatal("fresh copy was dropped")
		}
		s.copied(first, text("fresh"))
		if s.received(first, old, text("old")) {
			t.Fatal("stale transfer would overwrite fresh text")
		}
	}
}

func TestContentEchoAndRepeatedCopy(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if !s.received(waylandSide, generation, text("A")) {
		t.Fatal("initial copy was dropped")
	}
	s.copied(x11Side, text("A"))
	generation = s.changed(x11Side)
	if s.received(x11Side, generation, text("A")) {
		t.Fatal("echo would bounce back")
	}
	generation = s.changed(x11Side)
	if !s.received(x11Side, generation, text("B")) {
		t.Fatal("new content was dropped")
	}
	s.copied(waylandSide, text("B"))
	generation = s.changed(waylandSide)
	if !s.received(waylandSide, generation, text("A")) {
		t.Fatal("copying earlier content again was suppressed")
	}
}

func TestEchoComparesEveryFormat(t *testing.T) {
	var s syncState
	rich := clip{textKey: []byte("A"), "text/html": []byte("<b>A</b>")}
	generation := s.changed(waylandSide)
	if !s.received(waylandSide, generation, rich) {
		t.Fatal("rich copy was dropped")
	}
	s.copied(x11Side, rich)
	generation = s.changed(x11Side)
	if !s.received(x11Side, generation, text("A")) {
		t.Fatal("a copy without the HTML format was treated as an echo")
	}
}

func TestOwnerLossDuringOtherSideRead(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if _, mirror := s.emptied(x11Side); mirror {
		t.Fatal("owner loss cleared a copy that was still being read")
	}
	if !s.received(waylandSide, generation, text("fresh")) {
		t.Fatal("owner loss on the other side discarded the fresh copy")
	}
}

func TestOwnerLossKeepsCopyBeingPublished(t *testing.T) {
	for _, from := range []side{waylandSide, x11Side} {
		var s syncState
		generation := s.changed(1 - from)
		s.received(1-from, generation, text("old"))
		s.copied(from, text("old"))
		generation = s.changed(from)
		if !s.received(from, generation, text("new")) {
			t.Fatal("new copy was dropped")
		}
		// The old owner on the other side exits before the new copy arrives there.
		if _, mirror := s.emptied(1 - from); mirror {
			t.Fatal("owner loss cleared the new copy")
		}
		if !s.current(generation) {
			t.Fatal("owner loss cancelled the copy being published")
		}
	}
}

func TestVanishedOwnerCannotCompleteRead(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if _, mirror := s.emptied(waylandSide); mirror {
		t.Fatal("an unread copy was mirrored as a clear")
	}
	if s.received(waylandSide, generation, text("stale")) {
		t.Fatal("a vanished owner completed a stale transfer")
	}
}

func TestClearFollowsSyncedClipboard(t *testing.T) {
	for _, from := range []side{waylandSide, x11Side} {
		var s syncState
		generation := s.changed(from)
		s.received(from, generation, text("secret"))
		s.copied(1-from, text("secret"))
		generation, mirror := s.emptied(from)
		if !mirror || !s.current(generation) {
			t.Fatal("clearing a synced clipboard did not clear the other side")
		}
		s.copied(1-from, nil)
		if _, mirror := s.emptied(1 - from); mirror {
			t.Fatal("the mirrored clear bounced back")
		}
	}
}

func TestClearKeepsContentThatWasNotCopied(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	s.received(waylandSide, generation, text("A"))
	s.copied(x11Side, text("A"))
	generation = s.changed(x11Side)
	s.failed(generation)
	if _, mirror := s.emptied(waylandSide); mirror {
		t.Fatal("a clear removed X11 content that was never copied")
	}
}

func TestEmptyTextIsNotAnEmptyClipboard(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	s.received(waylandSide, generation, text("A"))
	s.copied(x11Side, text("A"))
	s.emptied(waylandSide)
	s.copied(x11Side, nil)
	generation = s.changed(waylandSide)
	if !s.received(waylandSide, generation, text("")) {
		t.Fatal("explicit empty text was suppressed")
	}
}

func TestFailedWriteDoesNotAcknowledgeDestination(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if !s.received(waylandSide, generation, text("text")) {
		t.Fatal("initial copy was dropped")
	}
	generation = s.changed(waylandSide)
	if !s.received(waylandSide, generation, text("text")) {
		t.Fatal("failed write was treated as a successful copy")
	}
}

func TestWantedFormats(t *testing.T) {
	offered := map[string]bool{"image/jpeg": true, "image/png": true, "text/plain": true, textKey: true, "text/uri-list": true, "text/html": true, "application/x-other": true}
	want := []string{textKey, "text/html", "text/uri-list", "image/png"}
	if got := wanted(offered, textMIMEs); !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := wanted(map[string]bool{"application/x-other": true}, textMIMEs); len(got) != 0 {
		t.Fatalf("unsupported types were selected: %q", got)
	}
}

func TestClipFormats(t *testing.T) {
	c := clip{"image/png": nil, textKey: nil, "text/uri-list": nil, "text/html": nil}
	want := []string{"text/html", "text/uri-list", "image/png"}
	if got := c.formats(); !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
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
	binary := []byte{0x89, 'P', 'N', 'G', 0, 0xff}
	if got, err := readData(bytes.NewReader(binary), 1024); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("binary data changed: error=%v", err)
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
