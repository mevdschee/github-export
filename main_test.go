package main

import (
	"testing"
	"time"
)

func TestParseMaxAge(t *testing.T) {
	now := time.Now()

	cases := []struct {
		in     string
		wantD  time.Duration
		wantOK bool
	}{
		{"12h", 12 * time.Hour, true},
		{"30d", 30 * 24 * time.Hour, true},
		{"4w", 4 * 7 * 24 * time.Hour, true},
		{"6mo", 6 * 30 * 24 * time.Hour, true},
		{"2y", 2 * 365 * 24 * time.Hour, true},
		{"1h", 1 * time.Hour, true},

		{"", 0, false},
		{"foo", 0, false},
		{"5x", 0, false},
		{"5m", 0, false},  // ambiguous — months use "mo"
		{"0d", 0, false},  // zero rejected
		{"-1d", 0, false}, // negative rejected
		{"1.5y", 0, false},
		{"y", 0, false},
		{"10", 0, false},
	}
	for _, tc := range cases {
		got, err := parseMaxAge(tc.in)
		if tc.wantOK {
			if err != nil {
				t.Errorf("parseMaxAge(%q): unexpected error %v", tc.in, err)
				continue
			}
			delta := now.Sub(got)
			// Allow 1s of slack since now is recomputed inside parseMaxAge.
			if delta < tc.wantD-time.Second || delta > tc.wantD+time.Second {
				t.Errorf("parseMaxAge(%q): delta %v, want ~%v", tc.in, delta, tc.wantD)
			}
		} else {
			if err == nil {
				t.Errorf("parseMaxAge(%q): want error, got %v", tc.in, got)
			}
		}
	}
}
