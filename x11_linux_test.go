package main

import (
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestX11SelectionRequestTime(t *testing.T) {
	for _, test := range []struct {
		request, owned xproto.Timestamp
		valid          bool
	}{
		{0, 100, true},
		{99, 100, false},
		{100, 100, true},
		{101, 100, true},
		{10, 0xfffffff0, true},
		{0xfffffff0, 10, false},
	} {
		if got := validXTime(test.request, test.owned); got != test.valid {
			t.Errorf("request=%d owned=%d: got %t, want %t", test.request, test.owned, got, test.valid)
		}
	}
}
