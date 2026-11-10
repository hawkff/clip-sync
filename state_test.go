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

// publishes reports whether st copies data to side to.
func publishes(st step, to side, data clip) bool {
	return st.kind == stepPublish && st.to == to && st.data.equal(data)
}

// synced returns a state in which side from copied content and the other side received it.
func synced(from side, content string) *syncState {
	var s syncState
	s.received(from, s.changed(from), text(content))
	s.copied(1-from, text(content))
	return &s
}

func TestStaleReadCannotOverwriteNewCopy(t *testing.T) {
	for _, first := range []side{waylandSide, x11Side} {
		var s syncState
		old := s.changed(first)
		fresh := s.changed(1 - first)
		// The first side changed too and was never read, so it is compared first.
		if st := s.received(1-first, fresh, text("fresh")); st.kind != stepVerify || st.to != first {
			t.Fatalf("fresh copy replaced an unread side: %+v", st)
		}
		if st := s.received(first, old, text("old")); st.kind != stepNone {
			t.Fatal("stale transfer would overwrite fresh text")
		}
		if st := s.received(first, fresh, text("old")); !publishes(st, first, text("fresh")) {
			t.Fatalf("fresh copy did not replace the older one: %+v", st)
		}
	}
}

func TestMirroredCopyLeavesBothSidesAlone(t *testing.T) {
	for _, first := range []side{waylandSide, x11Side} {
		var s syncState
		s.changed(first)
		// Another bridge puts the same copy on the other side.
		mirror := s.changed(1 - first)
		if st := s.received(1-first, mirror, text("copy")); st.kind != stepVerify || st.to != first {
			t.Fatalf("mirror was not compared with its source: %+v", st)
		}
		if st := s.received(first, mirror, text("copy")); st.kind != stepNone {
			t.Fatalf("bridge took over a copy both sides already hold: %+v", st)
		}
		if s.unsent != [2]bool{} {
			t.Fatal("a matched copy still counts as unsent")
		}
	}
}

func TestMirrorAfterReadLeavesBothSidesAlone(t *testing.T) {
	for _, first := range []side{waylandSide, x11Side} {
		s := synced(first, "old")
		generation := s.changed(first)
		if st := s.received(first, generation, text("copy")); !publishes(st, 1-first, text("copy")) {
			t.Fatalf("new copy was dropped: %+v", st)
		}
		// Another bridge puts the same copy on the other side before the bridge does.
		mirror := s.changed(1 - first)
		if st := s.received(1-first, mirror, text("copy")); st.kind != stepNone {
			t.Fatalf("bridge answered a mirror of a copy it already read: %+v", st)
		}
	}
}

func TestUnreadableSideIsReplacedByNewerCopy(t *testing.T) {
	var s syncState
	s.changed(x11Side)
	generation := s.changed(waylandSide)
	s.received(waylandSide, generation, text("fresh"))
	if st := s.failed(x11Side, generation); !publishes(st, x11Side, text("fresh")) {
		t.Fatalf("a side that cannot be read kept the fresh copy out: %+v", st)
	}
}

func TestUnreadableNewerSideGetsUnsentCopy(t *testing.T) {
	// Read before the other side changed.
	s := synced(waylandSide, "old")
	s.received(waylandSide, s.changed(waylandSide), text("fresh"))
	if st := s.failed(x11Side, s.changed(x11Side)); !publishes(st, x11Side, text("fresh")) {
		t.Fatalf("unsent copy was lost: %+v", st)
	}
	// Still unread when the other side changed.
	s = &syncState{}
	s.changed(waylandSide)
	if st := s.failed(x11Side, s.changed(x11Side)); st.kind != stepReread || st.to != waylandSide {
		t.Fatalf("unread copy was lost: %+v", st)
	}
	// Content from before the bridge started is not a copy to send.
	s = &syncState{}
	if st := s.failed(x11Side, s.changed(x11Side)); st.kind != stepNone {
		t.Fatalf("startup content replaced a new copy: %+v", st)
	}
}

func TestContentEchoAndRepeatedCopy(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if !publishes(s.received(waylandSide, generation, text("A")), x11Side, text("A")) {
		t.Fatal("initial copy was dropped")
	}
	s.copied(x11Side, text("A"))
	generation = s.changed(x11Side)
	if s.received(x11Side, generation, text("A")).kind != stepNone {
		t.Fatal("echo would bounce back")
	}
	generation = s.changed(x11Side)
	if !publishes(s.received(x11Side, generation, text("B")), waylandSide, text("B")) {
		t.Fatal("new content was dropped")
	}
	s.copied(waylandSide, text("B"))
	generation = s.changed(waylandSide)
	if !publishes(s.received(waylandSide, generation, text("A")), x11Side, text("A")) {
		t.Fatal("copying earlier content again was suppressed")
	}
}

func TestEchoComparesEveryFormat(t *testing.T) {
	var s syncState
	rich := clip{textKey: []byte("A"), "text/html": []byte("<b>A</b>")}
	generation := s.changed(waylandSide)
	if !publishes(s.received(waylandSide, generation, rich), x11Side, rich) {
		t.Fatal("rich copy was dropped")
	}
	s.copied(x11Side, rich)
	generation = s.changed(x11Side)
	if !publishes(s.received(x11Side, generation, text("A")), waylandSide, text("A")) {
		t.Fatal("a copy without the HTML format was treated as an echo")
	}
}

func TestOwnerLossDuringOtherSideRead(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if st := s.emptied(x11Side); st.kind != stepNone {
		t.Fatal("owner loss cleared a copy that was still being read")
	}
	if !publishes(s.received(waylandSide, generation, text("fresh")), x11Side, text("fresh")) {
		t.Fatal("owner loss on the other side discarded the fresh copy")
	}
}

func TestOwnerLossKeepsCopyBeingPublished(t *testing.T) {
	for _, from := range []side{waylandSide, x11Side} {
		s := synced(1-from, "old")
		generation := s.changed(from)
		if !publishes(s.received(from, generation, text("new")), 1-from, text("new")) {
			t.Fatal("new copy was dropped")
		}
		// The old owner on the other side exits before the new copy arrives
		// there. The copy goes out again, in case that exit dropped it.
		if st := s.emptied(1 - from); !publishes(st, 1-from, text("new")) {
			t.Fatalf("owner loss dropped or cleared the new copy: %+v", st)
		}
		if st := s.received(1-from, generation, text("stale")); st.kind != stepNone {
			t.Fatalf("a read from the vanished owner completed: %+v", st)
		}
	}
}

func TestSourceClearCancelsCopyBeingPublished(t *testing.T) {
	for _, from := range []side{waylandSide, x11Side} {
		s := synced(from, "old")
		generation := s.changed(from)
		s.received(from, generation, text("new"))
		// The source clears before the new copy reaches the other side.
		if st := s.emptied(from); !publishes(st, 1-from, nil) || s.current(generation) {
			t.Fatalf("a cleared source still published its copy: %+v", st)
		}
		// The same with an empty other side, which needs no clear.
		s.copied(1-from, nil)
		generation = s.changed(from)
		s.received(from, generation, text("newer"))
		if st := s.emptied(from); st.kind != stepNone || s.current(generation) {
			t.Fatalf("a cleared source still published its copy to an empty side: %+v", st)
		}
	}
}

func TestSupersededCopyIsNotRestored(t *testing.T) {
	for _, from := range []side{waylandSide, x11Side} {
		s := synced(from, "old")
		s.received(1-from, s.changed(1-from), text("first"))
		// A later copy on this side replaces the one still on its way here.
		s.received(from, s.changed(from), text("second"))
		if st := s.emptied(from); !publishes(st, 1-from, nil) {
			t.Fatalf("a clear brought back the copy it replaced: %+v", st)
		}
	}
}

func TestVanishedOwnerCannotCompleteRead(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if st := s.emptied(waylandSide); st.kind != stepNone {
		t.Fatal("an unread copy was mirrored as a clear")
	}
	if s.received(waylandSide, generation, text("stale")).kind != stepNone {
		t.Fatal("a vanished owner completed a stale transfer")
	}
}

func TestClearDuringComparison(t *testing.T) {
	for _, first := range []side{waylandSide, x11Side} {
		// The older side empties while it is compared: the newer copy goes there.
		var s syncState
		s.changed(first)
		generation := s.changed(1 - first)
		s.received(1-first, generation, text("fresh"))
		if st := s.emptied(first); !publishes(st, first, text("fresh")) {
			t.Fatalf("fresh copy was not sent to the emptied side: %+v", st)
		}
		if st := s.received(first, generation, text("stale")); st.kind != stepNone {
			t.Fatalf("the comparison completed after its side emptied: %+v", st)
		}
		// The newer side empties instead: the older, unread copy syncs.
		s = syncState{}
		s.changed(first)
		s.received(1-first, s.changed(1-first), text("fresh"))
		if st := s.emptied(1 - first); st.kind != stepReread || st.to != first {
			t.Fatalf("unread copy was cleared or lost: %+v", st)
		}
	}
}

func TestClearFollowsSyncedClipboard(t *testing.T) {
	for _, from := range []side{waylandSide, x11Side} {
		s := synced(from, "secret")
		if st := s.emptied(from); !publishes(st, 1-from, nil) {
			t.Fatal("clearing a synced clipboard did not clear the other side")
		}
		s.copied(1-from, nil)
		if st := s.emptied(1 - from); st.kind != stepNone {
			t.Fatal("the mirrored clear bounced back")
		}
	}
}

func TestClearKeepsContentThatWasNotCopied(t *testing.T) {
	s := synced(waylandSide, "A")
	if st := s.failed(x11Side, s.changed(x11Side)); st.kind != stepNone {
		t.Fatalf("content that cannot be copied was replaced: %+v", st)
	}
	if st := s.emptied(waylandSide); st.kind != stepNone {
		t.Fatal("a clear removed X11 content that was never copied")
	}
}

func TestEmptyTextIsNotAnEmptyClipboard(t *testing.T) {
	s := synced(waylandSide, "A")
	s.emptied(waylandSide)
	s.copied(x11Side, nil)
	generation := s.changed(waylandSide)
	if !publishes(s.received(waylandSide, generation, text("")), x11Side, text("")) {
		t.Fatal("explicit empty text was suppressed")
	}
}

func TestFailedWriteDoesNotAcknowledgeDestination(t *testing.T) {
	var s syncState
	generation := s.changed(waylandSide)
	if !publishes(s.received(waylandSide, generation, text("text")), x11Side, text("text")) {
		t.Fatal("initial copy was dropped")
	}
	generation = s.changed(waylandSide)
	if !publishes(s.received(waylandSide, generation, text("text")), x11Side, text("text")) {
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
