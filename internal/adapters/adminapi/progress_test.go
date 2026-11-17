package adminapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAWriteLargerThanAStallIsTakenAStepAtATime: a body held in memory
// reaches the progress writer in one Write. A client that takes it steadily,
// but slower than the whole of it in a stall, is not cut off: every step of
// the write has a stall of its own.
//
// The stall is three seconds, not one: the client here is a goroutine of a
// test that runs beside others under -race, and a runner that is busy can
// leave it unscheduled for more than a second - which is a client that
// really stopped, and is rightly cut off. The pause is set so that the body
// still takes longer than one stall (128 steps of 40 ms, some 5 s), which
// the last check holds to.
func TestAWriteLargerThanAStallIsTakenAStepAtATime(t *testing.T) {
	t.Parallel()
	const (
		stall = 3 * time.Second
		size  = 8 << 20
		step  = 64 << 10
		pause = 40 * time.Millisecond
	)
	body := bytes.Repeat([]byte("cactus"), size/6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		transfer := http.NewResponseController(w)
		_, _ = newProgressWriter(w, transfer, stall).Write(body)
	}))
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	begun := time.Now()
	var got bytes.Buffer
	for {
		n, err := io.CopyN(&got, resp.Body, step)
		if errors.Is(err, io.EOF) || (err == nil && n == 0) {
			break
		}
		if err != nil {
			t.Fatalf("after %d bytes: %v", got.Len(), err)
		}
		time.Sleep(pause)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("read %d of %d bytes", got.Len(), len(body))
	}
	if took := time.Since(begun); took <= stall {
		t.Fatalf("the body was taken in %s, within one stall: the test proves nothing", took)
	}
}
