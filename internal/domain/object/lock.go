package object

import (
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// Object Lock: write once, read many. A version can be kept from being
// removed until a date (retention) or until further notice (legal hold).
//
// The rules below are the whole of it. They are called by the use cases, to
// refuse early, and by the metadata state machine, which is where they are
// enforced: with the time of the entry that asks, never with a clock of the
// node that applies it. They are part of the stored format and do not change
// between releases.

// RetentionMode says who can end a retention period early.
type RetentionMode string

const (
	// Governance can be lifted by a caller with the permission to bypass
	// it.
	Governance RetentionMode = "GOVERNANCE"
	// Compliance can be lifted by nobody: not by the root credential, not
	// by an administrator, not by this code. It ends when its date has
	// passed.
	Compliance RetentionMode = "COMPLIANCE"
)

// maxRetentionYear is the last year a retention can end in.
const maxRetentionYear = 9999

// Retention keeps a version from being removed until a time.
type Retention struct {
	Mode  RetentionMode
	Until time.Time
}

// Validate reports whether r is a retention that can be set at the time at.
// The error wraps domain.ErrInvalidArgument.
func (r Retention) Validate(at time.Time) error {
	switch {
	case r.Mode != Governance && r.Mode != Compliance:
		return fmt.Errorf("%w: retention mode %q", domain.ErrInvalidArgument, r.Mode)
	case r.Until.IsZero():
		return fmt.Errorf("%w: retention without a date", domain.ErrInvalidArgument)
	case !r.Until.After(at):
		return fmt.Errorf("%w: the retain-until date is not in the future", domain.ErrInvalidArgument)
	case r.Until.UTC().Year() > maxRetentionYear:
		// A date no record can hold: timestamps are written with four
		// digits for the year.
		return fmt.Errorf("%w: the retain-until date is after the year %d", domain.ErrInvalidArgument, maxRetentionYear)
	}
	return nil
}

// Lock is what keeps a version from being removed.
type Lock struct {
	// Retention, when not nil, keeps the version until a time.
	Retention *Retention
	// LegalHold keeps the version until it is taken off, whatever the
	// retention says.
	LegalHold bool
}

// IsZero reports whether the lock holds nothing.
func (l Lock) IsZero() bool {
	return l.Retention == nil && !l.LegalHold
}

// active reports whether the retention still holds at the time at.
func (l Lock) active(at time.Time) bool {
	return l.Retention != nil && at.Before(l.Retention.Until)
}

// CheckDelete reports whether a version with this lock may be removed at the
// time at. bypassGovernance says that the caller may lift a governance
// retention and asked to. A refusal wraps domain.ErrObjectLocked.
//
// No argument makes a version under legal hold, or under a compliance
// retention that has not ended, removable.
func (l Lock) CheckDelete(at time.Time, bypassGovernance bool) error {
	switch {
	case l.LegalHold:
		return fmt.Errorf("%w: the version is under legal hold", domain.ErrObjectLocked)
	case !l.active(at):
		return nil
	case l.Retention.Mode == Compliance:
		return fmt.Errorf("%w: compliance retention until %s", domain.ErrObjectLocked, l.Retention.Until.UTC().Format(time.RFC3339))
	case !bypassGovernance:
		return fmt.Errorf("%w: governance retention until %s", domain.ErrObjectLocked, l.Retention.Until.UTC().Format(time.RFC3339))
	}
	return nil
}

// CheckRetention reports whether the retention of a version with this lock
// may be replaced by next (nil removes it) at the time at. A refusal wraps
// domain.ErrObjectLocked, or domain.ErrInvalidArgument for a retention that
// is not valid by itself.
//
// A retention can always be made longer, and governance can always be made
// compliance. While a compliance retention holds, that is all that can be
// done to it. While a governance retention holds, shortening or removing it
// needs the bypass.
func (l Lock) CheckRetention(next *Retention, at time.Time, bypassGovernance bool) error {
	if next != nil {
		if err := next.Validate(at); err != nil {
			return err
		}
	}
	if !l.active(at) {
		return nil
	}
	current := l.Retention
	extends := next != nil && !next.Until.Before(current.Until)
	switch current.Mode {
	case Compliance:
		if extends && next.Mode == Compliance {
			return nil
		}
		return fmt.Errorf("%w: a compliance retention can only be extended", domain.ErrObjectLocked)
	default:
		if extends || bypassGovernance {
			return nil
		}
		return fmt.Errorf("%w: a governance retention can only be extended without the permission to bypass it", domain.ErrObjectLocked)
	}
}
