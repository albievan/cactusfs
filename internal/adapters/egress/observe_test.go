package egress

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// observedKey marks the context an observer of a test hands back.
type observedKey struct{}

// recorder is an observer of a test: it records what it is told.
type recorder struct {
	mu      sync.Mutex
	started int
	// answered counts the answers, as the status the server of the test
	// gives every one.
	answered []int
	failed   []delivery.Class
}

// markedResolver resolves as names does, and counts the lookups made under
// a context an observer gave.
type markedResolver struct {
	names
	mu     *sync.Mutex
	marked *int
}

func (m markedResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if ctx.Value(observedKey{}) == true {
		m.mu.Lock()
		*m.marked++
		m.mu.Unlock()
	}
	return m.names.LookupNetIP(ctx, network, host)
}

func (o *recorder) observe(ctx context.Context) (context.Context, func(error)) {
	o.mu.Lock()
	o.started++
	o.mu.Unlock()
	return context.WithValue(ctx, observedKey{}, true), func(err error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if err != nil {
			o.failed = append(o.failed, delivery.ClassOf(err))
			return
		}
		o.answered = append(o.answered, http.StatusNoContent)
	}
}

// TestObserve: an observed client tells its observer of every request -
// an answer once its body is closed, and only once; a failure with its
// class - sends with the context the observer gave, through the transport
// of NewClient (an address it refuses is still refused), and leaves the
// client it was made from unobserved.
func TestObserve(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	var mu sync.Mutex
	marked := 0
	server := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	policy := allowLoopback(ca)
	policy.Resolver = markedResolver{names: names{"target.test": {loopback()}}, mu: &mu, marked: &marked}
	bare, _ := newClient(t, policy)
	o := &recorder{}
	observed := bare.Observe(o.observe)

	tests := []struct {
		name  string
		url   string
		class delivery.Class
	}{
		{name: "answered", url: "https://target.test:" + portOf(t, server.URL) + "/"},
		{name: "refused", url: "https://169.254.169.254/", class: delivery.ClassEgressRefused},
	}
	for _, tt := range tests {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := observed.Do(r)
		if tt.class != delivery.ClassNone {
			if c := delivery.ClassOf(err); c != tt.class || resp != nil {
				t.Errorf("%s: class %q, want %q (%v)", tt.name, c, tt.class, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		o.mu.Lock()
		early := len(o.answered)
		o.mu.Unlock()
		if early != 0 {
			t.Error("the observer was told of an answer before its body was closed")
		}
		_ = resp.Body.Close()
		_ = resp.Body.Close()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started != 2 || len(o.answered) != 1 || o.answered[0] != http.StatusNoContent ||
		len(o.failed) != 1 || o.failed[0] != delivery.ClassEgressRefused {
		t.Errorf("the observer: %d started, answered %v, failed %v", o.started, o.answered, o.failed)
	}
	mu.Lock()
	if marked == 0 {
		t.Error("the request was not sent with the context the observer gave")
	}
	mu.Unlock()

	if _, err := get(t, bare, "https://target.test:"+portOf(t, server.URL)+"/"); err != nil {
		t.Fatal(err)
	}
	if o.started != 2 {
		t.Error("a request of the client an observed one was made from was observed")
	}
}
