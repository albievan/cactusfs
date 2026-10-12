package delivery

import (
	"errors"
	"math"
	"time"
)

// Class is what a failure to deliver is called. It is all that is told of a
// failure outside the log of the node that leads: the words of the error
// and the endpoint never are (ADR-0025).
type Class string

// The classes of failure. A status shows them as they are written here; a
// newer release may add to them.
const (
	// ClassNone is no failure: the target took what it was sent.
	ClassNone Class = ""
	// ClassRejected is a target that refused this one message: a 4xx
	// other than 401, 403, 408 and 429 from a webhook, or a version the
	// other deployment refuses for itself. The row is passed and counted.
	ClassRejected Class = "rejected"
	// ClassSSEC is a version encrypted with a key of the customer's,
	// which is not the deployment's to use: it is never sent, and the row
	// is passed.
	ClassSSEC Class = "sse_c"
	// ClassTimeout is a target that did not answer in time.
	ClassTimeout Class = "timeout"
	// ClassConnect is a target that could not be connected to.
	ClassConnect Class = "connect"
	// ClassTLS is a handshake that failed, or a certificate that does not
	// chain to what the target says it must.
	ClassTLS Class = "tls"
	// ClassServer is a target that says the fault is its own or that it
	// is busy: a 5xx, a 408, a 429.
	ClassServer Class = "server"
	// ClassRedirect is a target that answered with a redirect, which is
	// never followed.
	ClassRedirect Class = "redirect"
	// ClassEgressRefused is an address the deployment does not connect to
	// (AllowedAddress).
	ClassEgressRefused Class = "egress_refused"
	// ClassAuth is a target that refuses the credential: a 401 or a 403,
	// a NATS server that does not take the token.
	ClassAuth Class = "auth"
	// ClassNoStream is a NATS subject that no stream takes.
	ClassNoStream Class = "no_stream"
	// ClassNoSuchBucket is another deployment that has no bucket of the
	// name the replication configuration gives.
	ClassNoSuchBucket Class = "no_such_bucket"
	// ClassVersioning is a bucket of the other deployment that does not
	// keep versions.
	ClassVersioning Class = "versioning"
	// ClassNotReplica is a bucket of the other deployment that is no
	// replica of this one - or a deployment that does not know what a
	// replica is.
	ClassNotReplica Class = "not_replica"
	// ClassQuota is a bucket of the other deployment that is full.
	ClassQuota Class = "quota"
	// ClassInternal is a failure on this side of the connection: the
	// version could not be read, the credential could not be opened.
	ClassInternal Class = "internal"
)

// maxClassLength bounds the word a class is.
const maxClassLength = 32

// ShownClass returns word as the class of a failure when it can be one - at
// most 32 lower-case letters and underscores, as the classes of this and of
// any later release are written - and ClassInternal otherwise. It is for
// whoever is told a class by another node and shows it: what arrives where
// a class belongs is shown only when it cannot be the words of an error, or
// an address.
func ShownClass(word string) Class {
	if len(word) > maxClassLength {
		return ClassInternal
	}
	for i := range len(word) {
		if c := word[i]; (c < 'a' || c > 'z') && c != '_' {
			return ClassInternal
		}
	}
	return Class(word)
}

// Passes reports whether an item that failed with this class is passed all
// the same: it is counted, never sent again, and the items behind it go on.
// Every other failure holds the target where it is and is tried again for as
// long as it takes: a target that takes nothing loses nothing until the
// outbox is at its bound.
func (c Class) Passes() bool {
	return c == ClassRejected || c == ClassSSEC
}

// WebhookClass returns what the answer of a webhook with this HTTP status
// is called. A 2xx is no failure. A 401 and a 403 are of the credential, not
// of the message: they are ClassAuth and are tried again, so that a secret
// that was mistyped or has expired stalls the target instead of passing
// every message undelivered.
func WebhookClass(status int) Class {
	switch {
	case status >= 200 && status <= 299:
		return ClassNone
	case status >= 300 && status <= 399:
		return ClassRedirect
	case status == 401 || status == 403:
		return ClassAuth
	case status == 408 || status == 429:
		return ClassServer
	case status >= 400 && status <= 499:
		return ClassRejected
	}
	// A 5xx, and anything that is no status at all.
	return ClassServer
}

// Error is a failure to deliver, with its class. Its text is the class and
// nothing else, so that it can be shown anywhere; what went wrong in so many
// words is the error it wraps, which is for the log of the node that leads
// and for nothing else.
type Error struct {
	// Class is what the failure is called.
	Class Class
	// Cause is the failure itself. It may name an endpoint.
	Cause error
}

// Error implements error. It never tells the cause.
func (e *Error) Error() string {
	return "delivery failed: " + string(e.Class)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error {
	return e.Cause
}

// Failed returns a failure of the class given. A cause that is nil, and a
// class that is none, still make a failure: ClassNone becomes
// ClassInternal.
func Failed(class Class, cause error) error {
	if class == ClassNone {
		class = ClassInternal
	}
	return &Error{Class: class, Cause: cause}
}

// ClassOf returns the class of a failure: ClassNone for no error, the class
// an Error carries, and ClassInternal for any other error.
func ClassOf(err error) Class {
	if err == nil {
		return ClassNone
	}
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Class
	}
	return ClassInternal
}

// The pauses between two attempts at a target that fails.
const (
	// BackoffMin is the pause after the first failure, before jitter.
	BackoffMin = time.Second
	// BackoffMax is the longest pause there is.
	BackoffMax = 5 * time.Minute
)

// Backoff returns how long to wait before the next attempt at a target that
// has failed attempt times in a row. The pause doubles from BackoffMin to
// BackoffMax; jitter, a number from 0 up to 1 that the caller draws at
// random, takes up to a quarter off it, so that targets that failed together
// are not tried together. The result is never longer than BackoffMax and
// never shorter than three quarters of BackoffMin.
func Backoff(attempt int, jitter float64) time.Duration {
	base := BackoffMax
	// Past the tenth failure the doubling is beyond the longest pause;
	// the shift is never one that overflows.
	if attempt < 1 {
		attempt = 1
	}
	if attempt <= 10 {
		base = min(BackoffMin<<(attempt-1), BackoffMax)
	}
	if math.IsNaN(jitter) || jitter < 0 {
		jitter = 0
	}
	jitter = min(jitter, 1)
	return base - time.Duration(float64(base/4)*jitter)
}
