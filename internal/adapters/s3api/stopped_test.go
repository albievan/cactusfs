package s3api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
)

// TestAStoppedStateMachineIsUnavailable: a node whose metadata state machine
// has stopped refuses every write before it is logged - certain, and another
// node can take it - so a client is told 503 ServiceUnavailable, for a single
// request and for each key of DeleteObjects, not 500. The failures that are
// the node's are logged once a request - how many, and the first cause - with
// the request and the operation and never a key; a key the client got wrong
// is not counted.
func TestAStoppedStateMachineIsUnavailable(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	s.putObject("/bkt/private-name-1", []byte("a"))
	// As raftmeta answers once its state machine has stopped.
	s.meta.FailWrites = fmt.Errorf("%w: metadata state machine stopped at entry 9: %w",
		domain.ErrUnavailable, errors.New("write state: no space left on device"))

	s.expectError(s.do(call{method: http.MethodDelete, path: "/bkt/private-name-1"}), http.StatusServiceUnavailable, "ServiceUnavailable")

	body := []byte(`<Delete>
		<Object><Key>private-name-1</Key></Object>
		<Object><Key>private-name-2</Key><VersionId>not-a-version</VersionId></Object>
		<Object><Key>private-name-3</Key></Object>
	</Delete>`)
	r := s.mustStatus(s.do(call{method: http.MethodPost, path: "/bkt?delete", body: body}), http.StatusOK)
	var parsed struct {
		Errors []struct{ Key, Code string } `xml:"Error"`
	}
	if err := xml.Unmarshal(r.body, &parsed); err != nil {
		t.Fatal(err)
	}
	want := []struct{ Key, Code string }{
		{"private-name-1", "ServiceUnavailable"}, {"private-name-2", "InvalidArgument"}, {"private-name-3", "ServiceUnavailable"},
	}
	if len(parsed.Errors) != len(want) || parsed.Errors[0] != want[0] || parsed.Errors[1] != want[1] || parsed.Errors[2] != want[2] {
		t.Fatalf("DeleteObjects errors = %+v, want %+v", parsed.Errors, want)
	}

	requestID := r.header.Get("X-Amz-Request-Id")
	var logged []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(s.logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if record["msg"] == "s3 delete of objects failed" {
			logged = append(logged, record)
		}
		if strings.Contains(line, "private-name") {
			t.Errorf("an object key is in the log: %s", line)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("%d lines of failed deletes logged, want 1 for the request: %v", len(logged), logged)
	}
	// Two keys failed for the node and one for the client: two are counted.
	if l := logged[0]; l["level"] != "ERROR" || l["request_id"] != requestID || l["operation"] != "DeleteObjects" ||
		l["failed"] != float64(2) || l["objects"] != float64(3) || !strings.Contains(fmt.Sprint(l["error"]), "no space left on device") {
		t.Errorf("the failures are logged as %v, want at ERROR with the request ID %s, the operation, 2 of 3 and the cause", l, requestID)
	}
}

// TestAnUnknownOutcomeIsNeverUnavailable: a write whose outcome is not known
// may have been committed, and must not look safe to repeat - even when its
// error also says that something was unavailable.
func TestAnUnknownOutcomeIsNeverUnavailable(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, domain.ErrUnavailable),
		fmt.Errorf("%w: %w", domain.ErrUnavailable, domain.ErrOutcomeUnknown),
		errors.Join(domain.ErrNotLeader, domain.ErrOutcomeUnknown),
	} {
		if got := toAPIError(err); got.status != http.StatusInternalServerError || got.code != "InternalError" {
			t.Errorf("toAPIError(%v) = %d %s, want 500 InternalError", err, got.status, got.code)
		}
	}
}
