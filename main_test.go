package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeduceOutDir(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{
			// Fresh run inside a checked-out repo (no prior export): must go
			// into github-data/, not pollute the checkout root. Regression for
			// the no-arg path that used to default to ".".
			name:  "fresh checkout",
			setup: func(t *testing.T, dir string) {},
			want:  "github-data",
		},
		{
			// `cd path/to/github-data && github-export`: the dir is itself an
			// export, so update it in place.
			name: "inside existing export",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "repo.yml"), "owner: x\nrepo: y\n")
			},
			want: ".",
		},
		{
			// Prior export lives in a github-data/ subfolder; reuse it.
			name: "existing github-data subfolder",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "github-data"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(dir, "github-data", "repo.yml"), "owner: x\nrepo: y\n")
			},
			want: "github-data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			chdir(t, dir)
			if got := deduceOutDir(); got != tc.want {
				t.Errorf("deduceOutDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

// chdir switches into dir for the duration of the test, restoring the previous
// working directory on cleanup. (t.Chdir is only available from Go 1.24.)
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Fatal(err)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

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
