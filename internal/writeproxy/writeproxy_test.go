package writeproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUpstream records the requests it receives and returns a canned response.
type fakeUpstream struct {
	srv      *httptest.Server
	lastAuth string
	lastBody string
	lastPath string
}

func newFakeUpstream(t *testing.T, status int, respBody string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		f.lastPath = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		f.lastBody = string(b)
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(status)
		w.Write([]byte(respBody))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newProxy(t *testing.T, f *fakeUpstream, resync ReSyncFunc) *Proxy {
	p := New("secret-token", resync)
	p.BaseURL = f.srv.URL
	return p
}

func TestForwardStreamsResponseAndAuth(t *testing.T) {
	f := newFakeUpstream(t, 200, `{"ok":true}`)
	p := newProxy(t, f, nil)

	req := httptest.NewRequest("GET", "/repos/o/r/actions/runs?per_page=5", nil)
	rec := httptest.NewRecorder()
	status := p.Forward(rec, req)

	if status != 200 {
		t.Errorf("status=%d, want 200", status)
	}
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body=%s", body)
	}
	if res.Header.Get("X-GitHub-Export-Proxied") != "true" {
		t.Errorf("missing proxied header")
	}
	if f.lastAuth != "Bearer secret-token" {
		t.Errorf("upstream auth=%q", f.lastAuth)
	}
	if f.lastPath != "/repos/o/r/actions/runs?per_page=5" {
		t.Errorf("upstream path=%q (query not preserved?)", f.lastPath)
	}
}

func TestForwardDisabledReturns501(t *testing.T) {
	p := New("t", nil)
	p.Disabled = true
	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	if status := p.Forward(rec, req); status != http.StatusNotImplemented {
		t.Errorf("status=%d, want 501", status)
	}
}

func TestForwardWriteTriggersResync(t *testing.T) {
	f := newFakeUpstream(t, 201, `{"number":42}`)
	var gotKind string
	var gotNum int64
	called := 0
	p := newProxy(t, f, func(kind string, number int64) error {
		called++
		gotKind, gotNum = kind, number
		return nil
	})

	req := httptest.NewRequest("PATCH", "/repos/o/r/issues/42", strings.NewReader(`{"state":"closed"}`))
	rec := httptest.NewRecorder()
	p.Forward(rec, req)

	if called != 1 || gotKind != "issue" || gotNum != 42 {
		t.Errorf("resync called=%d kind=%q num=%d, want 1/issue/42", called, gotKind, gotNum)
	}
	if f.lastBody != `{"state":"closed"}` {
		t.Errorf("upstream body=%q", f.lastBody)
	}
}

func TestForwardGetDoesNotResync(t *testing.T) {
	f := newFakeUpstream(t, 200, `{}`)
	called := 0
	p := newProxy(t, f, func(string, int64) error { called++; return nil })
	req := httptest.NewRequest("GET", "/repos/o/r/issues/42", nil)
	p.Forward(httptest.NewRecorder(), req)
	if called != 0 {
		t.Errorf("resync called %d times on GET, want 0", called)
	}
}

func TestForwardFailedWriteNoResync(t *testing.T) {
	f := newFakeUpstream(t, 422, `{"message":"validation failed"}`)
	called := 0
	p := newProxy(t, f, func(string, int64) error { called++; return nil })
	req := httptest.NewRequest("POST", "/repos/o/r/issues", strings.NewReader(`{}`))
	p.Forward(httptest.NewRecorder(), req)
	if called != 0 {
		t.Errorf("resync ran on a 422, want 0")
	}
}

func TestForwardResyncFailureSetsHeader(t *testing.T) {
	f := newFakeUpstream(t, 200, `{}`)
	p := newProxy(t, f, func(string, int64) error { return io.EOF })
	req := httptest.NewRequest("POST", "/repos/o/r/issues/42/comments", strings.NewReader(`{"body":"hi"}`))
	rec := httptest.NewRecorder()
	p.Forward(rec, req)
	if rec.Result().Header.Get("X-GitHub-Export-Resync") != "failed" {
		t.Errorf("expected resync-failed header")
	}
}

func TestRequestProgrammatic(t *testing.T) {
	f := newFakeUpstream(t, 201, `{"id":7}`)
	called := 0
	p := newProxy(t, f, func(kind string, number int64) error {
		called++
		if kind != "issues" {
			t.Errorf("kind=%q, want issues", kind)
		}
		return nil
	})
	status, body, err := p.Request(context.Background(), "POST", "/repos/o/r/issues", strings.NewReader(`{"title":"x"}`))
	if err != nil || status != 201 || string(body) != `{"id":7}` {
		t.Fatalf("Request → status=%d body=%s err=%v", status, body, err)
	}
	if called != 1 {
		t.Errorf("resync called=%d, want 1", called)
	}
	if f.lastBody != `{"title":"x"}` {
		t.Errorf("upstream body=%q", f.lastBody)
	}
}

func TestRequestDisabled(t *testing.T) {
	p := New("t", nil)
	p.Disabled = true
	status, _, err := p.Request(context.Background(), "POST", "/x", nil)
	if status != http.StatusNotImplemented || err == nil {
		t.Errorf("status=%d err=%v, want 501/err", status, err)
	}
}

func TestClassifyWrite(t *testing.T) {
	cases := []struct {
		method, path string
		wantKind     string
		wantNum      int64
		wantOK       bool
	}{
		{"PATCH", "/repos/o/r/issues/42", "issue", 42, true},
		{"POST", "/repos/o/r/issues/42/comments", "issue", 42, true},
		{"PUT", "/repos/o/r/pulls/9/merge", "issue", 9, true},
		{"PATCH", "/repos/o/r/pulls/9", "issue", 9, true},
		{"POST", "/repos/o/r/issues", "issues", 0, true},
		{"POST", "/repos/o/r/pulls", "issues", 0, true},
		{"POST", "/repos/o/r/labels", "labels", 0, true},
		{"GET", "/repos/o/r/issues/42", "issue", 42, true}, // classify is method-agnostic for issues
		{"POST", "/repos/o/r/actions/runs", "", 0, false},
	}
	for _, c := range cases {
		kind, num, ok := classifyWrite(c.method, c.path)
		if kind != c.wantKind || num != c.wantNum || ok != c.wantOK {
			t.Errorf("classifyWrite(%s %s) = %q,%d,%v want %q,%d,%v",
				c.method, c.path, kind, num, ok, c.wantKind, c.wantNum, c.wantOK)
		}
	}
}
