package object_test

import (
	"errors"
	"testing"
	"testing/quick"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

var lockNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func until(mode object.RetentionMode, d time.Duration) *object.Retention {
	return &object.Retention{Mode: mode, Until: lockNow.Add(d)}
}

func TestCheckDelete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		lock   object.Lock
		bypass bool
		want   error
	}{
		{"no lock", object.Lock{}, false, nil},
		{"governance, active", object.Lock{Retention: until(object.Governance, time.Hour)}, false, domain.ErrObjectLocked},
		{"governance, active, bypassed", object.Lock{Retention: until(object.Governance, time.Hour)}, true, nil},
		{"governance, ended", object.Lock{Retention: until(object.Governance, -time.Hour)}, false, nil},
		{"compliance, active", object.Lock{Retention: until(object.Compliance, time.Hour)}, false, domain.ErrObjectLocked},
		{"compliance, active, bypass asked", object.Lock{Retention: until(object.Compliance, time.Hour)}, true, domain.ErrObjectLocked},
		{"compliance, a nanosecond left", object.Lock{Retention: until(object.Compliance, time.Nanosecond)}, true, domain.ErrObjectLocked},
		{"compliance, ending this instant", object.Lock{Retention: until(object.Compliance, 0)}, false, nil},
		{"compliance, ended", object.Lock{Retention: until(object.Compliance, -time.Nanosecond)}, false, nil},
		{"legal hold", object.Lock{LegalHold: true}, false, domain.ErrObjectLocked},
		{"legal hold, bypass asked", object.Lock{LegalHold: true}, true, domain.ErrObjectLocked},
		{"legal hold over an ended retention", object.Lock{LegalHold: true, Retention: until(object.Governance, -time.Hour)}, true, domain.ErrObjectLocked},
		{"legal hold over a bypassable retention", object.Lock{LegalHold: true, Retention: until(object.Governance, time.Hour)}, true, domain.ErrObjectLocked},
	}
	for _, tt := range tests {
		if err := tt.lock.CheckDelete(lockNow, tt.bypass); !errors.Is(err, tt.want) {
			t.Errorf("%s: CheckDelete = %v, want %v", tt.name, err, tt.want)
		}
	}
	if (object.Lock{}).IsZero() != true || (object.Lock{LegalHold: true}).IsZero() {
		t.Error("IsZero")
	}
}

func TestCheckRetention(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		current *object.Retention
		next    *object.Retention
		bypass  bool
		want    error
	}{
		{"set on a version without one", nil, until(object.Compliance, time.Hour), false, nil},
		{"remove where there is none", nil, nil, false, nil},
		{"a date that has passed", nil, until(object.Governance, -time.Hour), false, domain.ErrInvalidArgument},
		{"a date that is now", nil, until(object.Governance, 0), false, domain.ErrInvalidArgument},
		{"no date", nil, &object.Retention{Mode: object.Governance}, false, domain.ErrInvalidArgument},
		{"a mode that does not exist", nil, &object.Retention{Mode: "FOREVER", Until: lockNow.Add(time.Hour)}, true, domain.ErrInvalidArgument},

		{"compliance extended", until(object.Compliance, time.Hour), until(object.Compliance, 2*time.Hour), false, nil},
		{"compliance, same date", until(object.Compliance, time.Hour), until(object.Compliance, time.Hour), false, nil},
		{"compliance shortened", until(object.Compliance, 2*time.Hour), until(object.Compliance, time.Hour), true, domain.ErrObjectLocked},
		{"compliance removed", until(object.Compliance, time.Hour), nil, true, domain.ErrObjectLocked},
		{"compliance made governance", until(object.Compliance, time.Hour), until(object.Governance, 2*time.Hour), true, domain.ErrObjectLocked},
		{"compliance that has ended, replaced", until(object.Compliance, -time.Hour), until(object.Governance, time.Hour), false, nil},
		{"compliance that has ended, removed", until(object.Compliance, -time.Hour), nil, false, nil},

		{"governance extended", until(object.Governance, time.Hour), until(object.Governance, 2*time.Hour), false, nil},
		{"governance made compliance, not earlier", until(object.Governance, time.Hour), until(object.Compliance, time.Hour), false, nil},
		{"governance made compliance, earlier", until(object.Governance, 2*time.Hour), until(object.Compliance, time.Hour), false, domain.ErrObjectLocked},
		{"governance shortened", until(object.Governance, 2*time.Hour), until(object.Governance, time.Hour), false, domain.ErrObjectLocked},
		{"governance shortened with bypass", until(object.Governance, 2*time.Hour), until(object.Governance, time.Hour), true, nil},
		{"governance removed", until(object.Governance, time.Hour), nil, false, domain.ErrObjectLocked},
		{"governance removed with bypass", until(object.Governance, time.Hour), nil, true, nil},
	}
	for _, tt := range tests {
		lock := object.Lock{Retention: tt.current}
		if err := lock.CheckRetention(tt.next, lockNow, tt.bypass); !errors.Is(err, tt.want) {
			t.Errorf("%s: CheckRetention = %v, want %v", tt.name, err, tt.want)
		}
	}
}

// TestComplianceCannotBeEscaped: whatever is asked, by whomever, a version
// under compliance retention that has not ended can be neither removed nor
// have its retention shortened, removed or weakened. The only change that is
// accepted leaves it at least as long, and compliance.
func TestComplianceCannotBeEscaped(t *testing.T) {
	t.Parallel()
	property := func(remaining uint32, offset int64, mode uint8, bypass, remove, hold bool) bool {
		lock := object.Lock{
			Retention: &object.Retention{Mode: object.Compliance, Until: lockNow.Add(time.Duration(remaining)*time.Second + time.Nanosecond)},
			LegalHold: hold,
		}
		if lock.CheckDelete(lockNow, bypass) == nil {
			return false
		}
		var next *object.Retention
		if !remove {
			next = &object.Retention{Mode: []object.RetentionMode{object.Governance, object.Compliance, "", "OTHER"}[mode%4], Until: lockNow.Add(time.Duration(offset % int64(200*365*24*time.Hour)))}
		}
		if lock.CheckRetention(next, lockNow, bypass) != nil {
			return true
		}
		return next != nil && next.Mode == object.Compliance && !next.Until.Before(lock.Retention.Until)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 20000}); err != nil {
		t.Error(err)
	}
}
