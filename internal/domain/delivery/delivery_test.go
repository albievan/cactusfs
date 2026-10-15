package delivery_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
)

func TestValidateTargetName(t *testing.T) {
	t.Parallel()
	for name, valid := range map[string]bool{
		"hook": true, "a": true, "0": true, "site-b": true, "a-": true, "a--b": true, strings.Repeat("a", 63): true,
		"": false, "-a": false, "Hook": false, "a.b": false, "a_b": false, "a b": false, "a/b": false, "a:b": false,
		"naïve": false, strings.Repeat("a", 64): false, "a\n": false, "*": false,
	} {
		err := delivery.ValidateTargetName(name)
		if (err == nil) != valid || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("ValidateTargetName(%q) = %v, valid wanted: %v", name, err, valid)
		}
	}
}

func TestEvents(t *testing.T) {
	t.Parallel()
	notified := map[delivery.Event]bool{
		delivery.EventPut: true, delivery.EventComplete: true, delivery.EventMarker: true, delivery.EventDelete: true,
		delivery.EventTags: false, delivery.EventRetention: false, delivery.EventLegalHold: false,
	}
	for event, want := range notified {
		if !event.Valid() || event.Notified() != want {
			t.Errorf("%q: valid = %v, notified = %v, want notified = %v", event, event.Valid(), event.Notified(), want)
		}
	}
	for _, event := range []delivery.Event{"", "PUT", "copy", "s3:ObjectCreated:Put"} {
		if event.Valid() || event.Notified() {
			t.Errorf("%q is taken for a kind of change", event)
		}
	}
}

func TestConsumersAndARNs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		consumer delivery.Consumer
		text     string
		arn      string
	}{
		{delivery.Consumer{Kind: delivery.ConsumerNotification, Target: "hook"}, "n:hook", "arn:cactusfs:notify:::hook"},
		{delivery.Consumer{Kind: delivery.ConsumerReplication, Target: "site-b"}, "r:site-b", "arn:cactusfs:replication:::site-b"},
	}
	for _, tt := range tests {
		if got := tt.consumer.String(); got != tt.text {
			t.Errorf("String() = %q, want %q", got, tt.text)
		}
		if got, err := delivery.ParseConsumer(tt.text); err != nil || got != tt.consumer {
			t.Errorf("ParseConsumer(%q) = %+v, %v", tt.text, got, err)
		}
		if got := tt.consumer.ARN(); got != tt.arn {
			t.Errorf("ARN() = %q, want %q", got, tt.arn)
		}
		if got, err := delivery.ParseARN(tt.arn); err != nil || got != tt.consumer {
			t.Errorf("ParseARN(%q) = %+v, %v", tt.arn, got, err)
		}
	}
	for _, text := range []string{"", "hook", "n:", ":hook", "x:hook", "n:Hook", "n:a:b", "N:hook", "n:hook ", "r:-a"} {
		if _, err := delivery.ParseConsumer(text); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ParseConsumer(%q) = %v, want %v", text, err, domain.ErrInvalidArgument)
		}
	}
	for _, arn := range []string{
		"", "hook", "arn:aws:sqs:eu-west-1:123456789012:queue", "arn:aws:sns:::hook", "arn:cactusfs:notify:::",
		"arn:cactusfs:notify:::Hook", "arn:cactusfs:notify::account:hook", "arn:cactusfs:notify:eu:1:hook",
		"arn:cactusfs:notify:::hook/x", "ARN:cactusfs:notify:::hook", "arn:cactusfs:replication:::a b",
		"arn:cactusfs:admin:::target/hook", " arn:cactusfs:notify:::hook",
	} {
		_, err := delivery.ParseARN(arn)
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ParseARN(%q) = %v, want %v", arn, err, domain.ErrInvalidArgument)
		}
	}
}

func FuzzParseARN(f *testing.F) {
	f.Add("arn:cactusfs:notify:::hook")
	f.Add("arn:cactusfs:replication:::site-b")
	f.Add("arn:aws:sqs:eu-west-1:123456789012:queue")
	f.Add("n:hook")
	f.Add("")
	f.Fuzz(func(t *testing.T, text string) {
		consumer, err := delivery.ParseARN(text)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParseARN: %v", err)
			}
		} else if consumer.ARN() != text || delivery.ValidateTargetName(consumer.Target) != nil {
			// What is read is what is written, and names a target.
			t.Fatalf("ParseARN(%q) = %+v, which is written %q", text, consumer, consumer.ARN())
		}
		// The same for the form a row of the outbox has.
		row, err := delivery.ParseConsumer(text)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParseConsumer: %v", err)
			}
			return
		}
		if row.String() != text {
			t.Fatalf("ParseConsumer(%q) = %+v, which is written %q", text, row, row.String())
		}
		if again, err := delivery.ParseARN(row.ARN()); err != nil || again != row {
			t.Fatalf("the ARN of %+v reads as %+v, %v", row, again, err)
		}
	})
}

func TestClasses(t *testing.T) {
	t.Parallel()
	passes := map[delivery.Class]bool{
		delivery.ClassRejected: true, delivery.ClassSSEC: true,
		delivery.ClassNone: false, delivery.ClassTimeout: false, delivery.ClassConnect: false, delivery.ClassTLS: false,
		delivery.ClassServer: false, delivery.ClassRedirect: false, delivery.ClassEgressRefused: false, delivery.ClassAuth: false,
		delivery.ClassNoStream: false, delivery.ClassNoSuchBucket: false, delivery.ClassVersioning: false,
		delivery.ClassNotReplica: false, delivery.ClassQuota: false, delivery.ClassInternal: false,
	}
	for class, want := range passes {
		if class.Passes() != want {
			t.Errorf("%q passes = %v, want %v", class, class.Passes(), want)
		}
	}
	for status, want := range map[int]delivery.Class{
		200: delivery.ClassNone, 201: delivery.ClassNone, 204: delivery.ClassNone, 299: delivery.ClassNone,
		301: delivery.ClassRedirect, 302: delivery.ClassRedirect, 304: delivery.ClassRedirect, 307: delivery.ClassRedirect,
		// A receiver that refuses the credential is not one that refuses a
		// message: nothing is passed.
		401: delivery.ClassAuth, 403: delivery.ClassAuth,
		400: delivery.ClassRejected, 404: delivery.ClassRejected, 410: delivery.ClassRejected, 413: delivery.ClassRejected, 422: delivery.ClassRejected,
		408: delivery.ClassServer, 429: delivery.ClassServer,
		500: delivery.ClassServer, 502: delivery.ClassServer, 503: delivery.ClassServer, 599: delivery.ClassServer,
		0: delivery.ClassServer, 100: delivery.ClassServer, 199: delivery.ClassServer, 600: delivery.ClassServer, -1: delivery.ClassServer,
	} {
		if got := delivery.WebhookClass(status); got != want {
			t.Errorf("WebhookClass(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestShownClass(t *testing.T) {
	t.Parallel()
	for word, want := range map[string]delivery.Class{
		"": delivery.ClassNone, "timeout": delivery.ClassTimeout, "no_such_bucket": delivery.ClassNoSuchBucket,
		// A class of a later release is a word like the others.
		"throttled": "throttled", strings.Repeat("a", 32): delivery.Class(strings.Repeat("a", 32)),
		// The words of an error, and an address, are not.
		"dial tcp 10.0.0.1:443: i/o timeout": delivery.ClassInternal, "https://hooks.example.com": delivery.ClassInternal,
		"Timeout": delivery.ClassInternal, "sse-c": delivery.ClassInternal, "a1": delivery.ClassInternal, "tls\n": delivery.ClassInternal,
		strings.Repeat("a", 33): delivery.ClassInternal,
	} {
		if got := delivery.ShownClass(word); got != want {
			t.Errorf("ShownClass(%q) = %q, want %q", word, got, want)
		}
	}
	// Every class of this release is one that is shown as it is.
	for _, class := range []delivery.Class{
		delivery.ClassNone, delivery.ClassRejected, delivery.ClassSSEC, delivery.ClassTimeout, delivery.ClassConnect, delivery.ClassTLS,
		delivery.ClassServer, delivery.ClassRedirect, delivery.ClassEgressRefused, delivery.ClassAuth, delivery.ClassNoStream,
		delivery.ClassNoSuchBucket, delivery.ClassVersioning, delivery.ClassNotReplica, delivery.ClassQuota, delivery.ClassInternal,
	} {
		if got := delivery.ShownClass(string(class)); got != class {
			t.Errorf("the class %q is shown as %q", class, got)
		}
	}
}

// TestFailuresTellTheirClassAndNothingElse: the text of a failure is its
// class. The cause - which may name an endpoint, or repeat what a server
// said - is there for the log and in no text.
func TestFailuresTellTheirClassAndNothingElse(t *testing.T) {
	t.Parallel()
	const mark = "MARK-41c7"
	cause := fmt.Errorf("post \"https://%s.example/hook\": dial tcp: lookup %s", mark, mark)
	err := delivery.Failed(delivery.ClassConnect, cause)
	if delivery.ClassOf(err) != delivery.ClassConnect || !errors.Is(err, cause) {
		t.Errorf("Failed = %v of class %q", err, delivery.ClassOf(err))
	}
	wrapped := fmt.Errorf("delivery.Worker: target hook: %w", err)
	if delivery.ClassOf(wrapped) != delivery.ClassConnect {
		t.Errorf("a failure that was wrapped is of class %q", delivery.ClassOf(wrapped))
	}
	for _, text := range []string{err.Error(), wrapped.Error(), fmt.Sprintf("%v %+v %s %q", err, err, err, err)} {
		if strings.Contains(text, mark) || !strings.Contains(text, string(delivery.ClassConnect)) {
			t.Errorf("the text of a failure: %q", text)
		}
	}
	if got := delivery.ClassOf(nil); got != delivery.ClassNone {
		t.Errorf("ClassOf(nil) = %q", got)
	}
	if got := delivery.ClassOf(errors.New("too many open files")); got != delivery.ClassInternal {
		t.Errorf("ClassOf of another error = %q", got)
	}
	// A failure is one whatever it is made of.
	none := delivery.Failed(delivery.ClassNone, nil)
	if none == nil || delivery.ClassOf(none) != delivery.ClassInternal || errors.Unwrap(none) != nil {
		t.Errorf("Failed(none, nil) = %v", none)
	}
}

// TestAFailureSaysWhetherItIsATimeout: what net.Error and url.Error ask of
// an error is answered by its class.
func TestAFailureSaysWhetherItIsATimeout(t *testing.T) {
	t.Parallel()
	for class, want := range map[delivery.Class]bool{
		delivery.ClassTimeout: true, delivery.ClassConnect: false, delivery.ClassTLS: false, delivery.ClassServer: false,
	} {
		var timeout interface{ Timeout() bool }
		if err := delivery.Failed(class, errors.New("cause")); !errors.As(err, &timeout) || timeout.Timeout() != want {
			t.Errorf("a failure of class %q: Timeout() = %v, want %v", class, timeout != nil && timeout.Timeout(), want)
		}
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0, time.Second},
		{2, 0, 2 * time.Second},
		{3, 0, 4 * time.Second},
		{9, 0, 256 * time.Second},
		{10, 0, 5 * time.Minute},
		{11, 0, 5 * time.Minute},
		{1000, 0, 5 * time.Minute},
		{math.MaxInt, 0, 5 * time.Minute},
		{0, 0, time.Second},
		{-5, 0, time.Second},
		{1, 1, 750 * time.Millisecond},
		{3, 0.5, 3500 * time.Millisecond},
		{10, 1, 225 * time.Second},
		// A jitter that is none is no jitter, and one beyond 1 is 1.
		{2, -1, 2 * time.Second},
		{2, math.NaN(), 2 * time.Second},
		{2, math.Inf(1), 1500 * time.Millisecond},
		{2, 7, 1500 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := delivery.Backoff(tt.attempt, tt.jitter); got != tt.want {
			t.Errorf("Backoff(%d, %v) = %v, want %v", tt.attempt, tt.jitter, got, tt.want)
		}
	}
	// Whatever the attempt and the jitter: within bounds, and never shorter
	// for a later attempt with the same jitter.
	for _, jitter := range []float64{0, 0.1, 0.5, 0.999, 1} {
		previous := time.Duration(0)
		for attempt := range 80 {
			got := delivery.Backoff(attempt, jitter)
			if got < delivery.BackoffMin*3/4 || got > delivery.BackoffMax || got < previous {
				t.Fatalf("Backoff(%d, %v) = %v after %v", attempt, jitter, got, previous)
			}
			previous = got
		}
	}
}
