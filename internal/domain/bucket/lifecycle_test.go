package bucket_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// day returns a time of day, "15:04:05", on the n-th of March 2026, UTC.
func day(n int, clock string) time.Time {
	var h, m, sec int
	_, _ = fmt.Sscanf(clock, "%d:%d:%d", &h, &m, &sec)
	return time.Date(2026, time.March, n, h, m, sec, 0, time.UTC)
}

func TestLifecycleValidate(t *testing.T) {
	t.Parallel()
	one := func(r bucket.LifecycleRule) bucket.Lifecycle {
		return bucket.Lifecycle{Rules: []bucket.LifecycleRule{r}}
	}
	many := bucket.Lifecycle{}
	for range 1001 {
		many.Rules = append(many.Rules, bucket.LifecycleRule{Enabled: true, ExpirationDays: 1})
	}
	tests := []struct {
		name  string
		rules bucket.Lifecycle
		ok    bool
	}{
		{"expire after days", one(bucket.LifecycleRule{ID: "a", Enabled: true, Prefix: "logs/", ExpirationDays: 30}), true},
		{"expire on a date", one(bucket.LifecycleRule{ExpirationDate: day(1, "00:00:00")}), true},
		{"old versions and uploads", one(bucket.LifecycleRule{NoncurrentDays: 7, AbortIncompleteDays: 2}), true},
		{"lone markers, with old versions", one(bucket.LifecycleRule{ExpiredObjectDeleteMarker: true, NoncurrentDays: 1}), true},
		{"a disabled rule is still a rule", one(bucket.LifecycleRule{Enabled: false, ExpirationDays: 1}), true},
		{"no rules", bucket.Lifecycle{}, false},
		{"too many rules", many, false},
		{"a rule that does nothing", one(bucket.LifecycleRule{Enabled: true, Prefix: "x"}), false},
		{"days and a date", one(bucket.LifecycleRule{ExpirationDays: 1, ExpirationDate: day(1, "00:00:00")}), false},
		{"days and lone markers", one(bucket.LifecycleRule{ExpirationDays: 1, ExpiredObjectDeleteMarker: true}), false},
		{"a date that is not midnight", one(bucket.LifecycleRule{ExpirationDate: day(1, "12:00:00")}), false},
		{"more days than a rule may name", one(bucket.LifecycleRule{ExpirationDays: bucket.MaxLifecycleDays + 1}), false},
		{"more noncurrent days than that", one(bucket.LifecycleRule{NoncurrentDays: 200000}), false},
		{"more abort days than that", one(bucket.LifecycleRule{AbortIncompleteDays: 1 << 40}), false},
		{"the most days a rule may name", one(bucket.LifecycleRule{ExpirationDays: bucket.MaxLifecycleDays}), true},
		{"negative days", one(bucket.LifecycleRule{ExpirationDays: -1, NoncurrentDays: 1}), false},
		{"negative noncurrent days", one(bucket.LifecycleRule{NoncurrentDays: -1, ExpirationDays: 1}), false},
		{"negative abort days", one(bucket.LifecycleRule{AbortIncompleteDays: -1, ExpirationDays: 1}), false},
		{"a long ID", one(bucket.LifecycleRule{ID: strings.Repeat("x", 256), ExpirationDays: 1}), false},
		{"a long prefix", one(bucket.LifecycleRule{Prefix: strings.Repeat("x", 1025), ExpirationDays: 1}), false},
		{"an ID twice", bucket.Lifecycle{Rules: []bucket.LifecycleRule{{ID: "a", ExpirationDays: 1}, {ID: "a", ExpirationDays: 2}}}, false},
	}
	for _, tt := range tests {
		err := tt.rules.Validate()
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	if !bucket.ValidConfigKind(bucket.ConfigLifecycle) {
		t.Error("lifecycle is a kind of configuration")
	}
}

// TestLifecycleDecisions: when each kind of rule says something is due. Days
// are counted as S3 counts them: to the first midnight UTC that is at least
// that many days after.
func TestLifecycleDecisions(t *testing.T) {
	t.Parallel()
	rules := bucket.Lifecycle{Rules: []bucket.LifecycleRule{
		{ID: "logs", Enabled: true, Prefix: "logs/", ExpirationDays: 2, NoncurrentDays: 1, AbortIncompleteDays: 3},
		{ID: "off", Enabled: false, Prefix: "", ExpirationDays: 1, NoncurrentDays: 1, AbortIncompleteDays: 1, ExpiredObjectDeleteMarker: false},
		{ID: "tmp", Enabled: true, Prefix: "tmp/", ExpirationDate: day(10, "00:00:00")},
		{ID: "markers", Enabled: true, Prefix: "logs/app/", ExpiredObjectDeleteMarker: true},
	}}
	made := day(5, "09:30:00")
	current := []struct {
		key  string
		now  time.Time
		want bool
	}{
		{"logs/a", day(7, "09:30:00"), false}, // two days later to the second: not yet midnight
		{"logs/a", day(7, "23:59:59"), false},
		{"logs/a", day(8, "00:00:00"), true},
		{"logs/a", day(20, "00:00:00"), true},
		{"other/a", day(20, "00:00:00"), false}, // only the disabled rule is about it
		{"log", day(20, "00:00:00"), false},
		{"tmp/x", day(9, "23:59:59"), false},
		{"tmp/x", day(10, "00:00:00"), true},
	}
	for _, tt := range current {
		if got := rules.ExpiresCurrent(tt.key, made, tt.now); got != tt.want {
			t.Errorf("ExpiresCurrent(%q, made %v, now %v) = %v", tt.key, made, tt.now, got)
		}
	}
	// Made at midnight exactly, a day is a day.
	if !rules.ExpiresCurrent("logs/a", day(5, "00:00:00"), day(7, "00:00:00")) || rules.ExpiresCurrent("logs/a", day(5, "00:00:00"), day(6, "23:59:59")) {
		t.Error("an object made at midnight")
	}
	if rules.ExpiresNoncurrent("logs/a", made, day(6, "23:00:00")) || !rules.ExpiresNoncurrent("logs/a", made, day(7, "00:00:00")) || rules.ExpiresNoncurrent("tmp/a", made, day(20, "00:00:00")) {
		t.Error("ExpiresNoncurrent")
	}
	if rules.AbortsUpload("logs/a", made, day(8, "23:00:00")) || !rules.AbortsUpload("logs/a", made, day(9, "00:00:00")) || rules.AbortsUpload("x", made, day(20, "00:00:00")) {
		t.Error("AbortsUpload")
	}
	if !rules.RemovesLoneMarker("logs/app/x") || rules.RemovesLoneMarker("logs/x") || rules.RemovesLoneMarker("x") {
		t.Error("RemovesLoneMarker")
	}
	if (bucket.Lifecycle{}).ExpiresCurrent("k", made, day(20, "00:00:00")) {
		t.Error("no rules expire nothing")
	}
}

// TestLifecycleNeverActsEarly: nothing is due before the number of days has
// passed in full, whatever the times; a disabled rule and a rule about
// another prefix never make anything due.
func TestLifecycleNeverActsEarly(t *testing.T) {
	t.Parallel()
	base := day(1, "00:00:00")
	property := func(days uint32, madeAfter, nowAfter uint32, enabled bool, near bool) bool {
		// Any number of days a rule may name - and, half of the time, a
		// small one, where a few seconds decide.
		d := int(days%bucket.MaxLifecycleDays) + 1
		if near {
			d = int(days%60) + 1
		}
		rule := bucket.LifecycleRule{Enabled: enabled, Prefix: "in/", ExpirationDays: d, NoncurrentDays: d, AbortIncompleteDays: d}
		rules := bucket.Lifecycle{Rules: []bucket.LifecycleRule{rule}}
		made := base.Add(time.Duration(madeAfter%1_000_000) * time.Second)
		now := made.Add(time.Duration(nowAfter%10_000_000) * time.Second)
		full := now.Sub(made) >= time.Duration(d)*24*time.Hour
		late := now.Sub(made) >= time.Duration(d+1)*24*time.Hour
		for _, due := range []bool{rules.ExpiresCurrent("in/k", made, now), rules.ExpiresNoncurrent("in/k", made, now), rules.AbortsUpload("in/k", made, now)} {
			if due && (!enabled || !full) {
				return false
			}
			// And never later than a day after: the next midnight.
			if enabled && late && !due {
				return false
			}
		}
		return !rules.ExpiresCurrent("out/k", made, now) && !rules.ExpiresNoncurrent("out/k", made, now) && !rules.AbortsUpload("out/k", made, now)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}

// TestLifecycleAndTimesThatCannotBe: a number of days that would overflow
// the arithmetic of durations, were it ever stored, makes nothing due; nor
// does a record that has lost its time.
func TestLifecycleAndTimesThatCannotBe(t *testing.T) {
	t.Parallel()
	made, now := day(5, "09:30:00"), day(5, "09:31:00")
	for _, days := range []int{106751, 106752, 110000, 200000, 213504, 2147483647, 1 << 40} {
		rules := bucket.Lifecycle{Rules: []bucket.LifecycleRule{{Enabled: true, ExpirationDays: days, NoncurrentDays: days, AbortIncompleteDays: days}}}
		if rules.ExpiresCurrent("k", made, now) || rules.ExpiresNoncurrent("k", made, now) || rules.AbortsUpload("k", made, now) {
			t.Errorf("something a minute old is due under a rule of %d days", days)
		}
		if days > bucket.MaxLifecycleDays && rules.Validate() == nil {
			t.Errorf("a rule of %d days is accepted", days)
		}
	}
	rules := bucket.Lifecycle{Rules: []bucket.LifecycleRule{
		{Enabled: true, ExpirationDays: 1, NoncurrentDays: 1, AbortIncompleteDays: 1},
		{Enabled: true, Prefix: "dated/", ExpirationDate: day(1, "00:00:00")},
	}}
	var lost time.Time
	far := day(28, "00:00:00")
	if rules.ExpiresCurrent("k", lost, far) || rules.ExpiresNoncurrent("k", lost, far) || rules.AbortsUpload("k", lost, far) || rules.ExpiresCurrent("dated/k", lost, far) {
		t.Error("a record without a time is due")
	}
	// The most days a rule may name is still counted rightly.
	long := bucket.Lifecycle{Rules: []bucket.LifecycleRule{{Enabled: true, ExpirationDays: bucket.MaxLifecycleDays}}}
	if long.ExpiresCurrent("k", made, made.AddDate(0, 0, bucket.MaxLifecycleDays-1)) || !long.ExpiresCurrent("k", made, made.AddDate(0, 0, bucket.MaxLifecycleDays+1)) {
		t.Error("a rule of the most days there are")
	}
}
