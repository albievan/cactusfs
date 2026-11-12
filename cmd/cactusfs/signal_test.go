package main

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestShutdownContextReleasesTheSignals: once the first signal has ended the
// context, the signals are handed back so that a second one ends the
// process, even while the graceful shutdown still runs.
func TestShutdownContextReleasesTheSignals(t *testing.T) {
	t.Parallel()

	var (
		asked   []os.Signal
		stopped atomic.Int32
		arrive  context.CancelFunc
	)
	notify := func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
		asked = signals
		ctx, cancel := context.WithCancel(parent)
		arrive = cancel
		return ctx, func() { stopped.Add(1); cancel() }
	}
	ctx, stop := shutdownContext(t.Context(), notify)
	defer stop()

	if !slices.Equal(asked, []os.Signal{os.Interrupt, syscall.SIGTERM}) {
		t.Errorf("signals = %v, want SIGINT and SIGTERM", asked)
	}
	if stopped.Load() != 0 {
		t.Fatal("the signals were released before any arrived")
	}
	arrive() // the first signal
	<-ctx.Done()
	deadline := time.Now().Add(10 * time.Second)
	for stopped.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the signals were not released once the first had arrived: a second could not end the process")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestShutdownContextWithTheRealSignals: the context of main ends with its
// parent and is then released without trouble.
func TestShutdownContextWithTheRealSignals(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(t.Context())
	ctx, stop := shutdownContext(parent, signal.NotifyContext)
	defer stop()
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the context did not end with its parent")
	}
}
