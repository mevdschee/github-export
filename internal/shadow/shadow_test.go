package shadow

import (
	"context"
	"sync"
	"testing"
)

func TestNormalizeStripsVolatileAndOrders(t *testing.T) {
	c := New(nil, nil, "o/r")
	a := c.normalize([]byte(`{"number":1,"url":"https://x","title":"hi"}`))
	b := c.normalize([]byte(`{"title":"hi","number":1,"url":"https://y"}`))
	if string(a) != string(b) {
		t.Errorf("normalize not order/volatile-insensitive:\n a=%s\n b=%s", a, b)
	}
}

func TestCheckDedupAndFiles(t *testing.T) {
	var mu sync.Mutex
	filed := 0
	fetch := func(_ context.Context, _ string) (int, []byte, error) {
		return 200, []byte(`{"number":1,"title":"REMOTE"}`), nil
	}
	file := func(title, body string) error {
		mu.Lock()
		filed++
		mu.Unlock()
		return nil
	}
	c := New(fetch, file, "o/r")
	local := []byte(`{"number":1,"title":"LOCAL"}`)
	c.Check("/repos/o/r/issues/1", local)
	c.Check("/repos/o/r/issues/1", local) // same fingerprint → deduped
	mu.Lock()
	defer mu.Unlock()
	if filed != 1 {
		t.Errorf("filed=%d, want 1 (deduped)", filed)
	}
}

func TestCheckNoDivergenceNoFile(t *testing.T) {
	filed := 0
	fetch := func(_ context.Context, _ string) (int, []byte, error) {
		return 200, []byte(`{"number":1,"url":"https://remote","title":"same"}`), nil
	}
	file := func(_, _ string) error { filed++; return nil }
	c := New(fetch, file, "o/r")
	// Differs only by the volatile url field → no divergence after normalize.
	c.Check("/x", []byte(`{"number":1,"url":"https://local","title":"same"}`))
	if filed != 0 {
		t.Errorf("filed=%d, want 0 (only volatile diff)", filed)
	}
}
