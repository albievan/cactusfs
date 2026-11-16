package telemetry_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// TestEgressDialsAreCountedByClass: a connection the dial function of the
// egress client cannot make is counted by its class in the counter of
// requests that got no answer, one it makes is not, every dial is a span -
// and nothing of the address or the words of the error is in a metric or a
// span.
func TestEgressDialsAreCountedByClass(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	const address = "MARK-nats.example:4222"

	if tel.InstrumentEgressDial(nil) != nil {
		t.Error("no dial function was given one")
	}
	tests := []struct {
		name    string
		failure error
	}{
		{name: "refused by the policy", failure: delivery.Failed(delivery.ClassEgressRefused, errors.New("MARK-nats refused"))},
		{name: "refused again", failure: &net.OpError{Op: "dial", Net: "tcp", Err: delivery.Failed(delivery.ClassEgressRefused, errors.New("MARK"))}},
		{name: "nothing listens", failure: delivery.Failed(delivery.ClassConnect, errors.New("connection refused: MARK-nats"))},
		{name: "no class at all", failure: errors.New("MARK-nats strange")},
		{name: "connected"},
	}
	for _, tt := range tests {
		var seen string
		local, remote := net.Pipe()
		dial := tel.InstrumentEgressDial(func(_ context.Context, _, address string) (net.Conn, error) {
			seen = address
			if tt.failure != nil {
				return nil, tt.failure
			}
			return local, nil
		})
		conn, err := dial(t.Context(), "tcp", address)
		switch {
		case seen != address:
			t.Errorf("%s: the dial function was given %q", tt.name, seen)
		case tt.failure == nil && (err != nil || conn != local):
			t.Errorf("%s: Dial = %v, %v", tt.name, conn, err)
		case tt.failure != nil && (!errors.Is(err, tt.failure) || conn != nil):
			t.Errorf("%s: Dial = %v, %v; want %v", tt.name, conn, err, tt.failure)
		}
		_ = local.Close()
		_ = remote.Close()
	}
	got := gauges(t, tel)
	for class, want := range map[string]float64{"egress_refused": 2, "connect": 1, "internal": 1, "timeout": 0} {
		if value := got["cactusfs_egress_refusals_total{class="+class+"}"]; value != want {
			t.Errorf("refusals of class %s = %v, want %v", class, value, want)
		}
	}
	dials := 0
	for _, name := range spanNames(recorder) {
		if name == "egress.Dial" {
			dials++
		}
	}
	if dials != len(tests) {
		t.Errorf("%d spans named egress.Dial, want %d", dials, len(tests))
	}
	wantNothingOf(t, tel, recorder, "MARK", "nats.example", "4222")
}
