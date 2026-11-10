package kubernetes

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain/release"
)

// The annotations and the label the upgrader writes.
const (
	// LockAnnotation, on the StatefulSet of the first node (by name),
	// holds the run under way: written with the StatefulSet's
	// resourceVersion, so that of two gateways that start at once one
	// wins, and removed when the run ends.
	LockAnnotation = "cactusfs.io/upgrade"
	// ResultAnnotation, beside it, holds how the last run ended, written
	// as the lock is removed: what Progress says once nothing runs.
	ResultAnnotation = "cactusfs.io/upgrade-result"
	// PreviousAnnotation, on the StatefulSet of a node that was moved,
	// holds the image and version it ran before: what it is put back to
	// when it does not come up, by this run or by one that resumes it.
	PreviousAnnotation = "cactusfs.io/upgrade-previous"
	// VersionLabel is the label of the pod template that says the version
	// the pod runs (the chart's).
	VersionLabel = "app.kubernetes.io/version"
)

// lockRecord is the value of LockAnnotation.
type lockRecord struct {
	// Run names the run: random, so that a holder knows its own lock.
	Run string `json:"run"`
	// Target is the version being installed.
	Target string `json:"target"`
	// Image is the image every node is moved to: <repository>@<digest>.
	Image string `json:"image"`
	// Holder is the node that runs it, which moves itself last.
	Holder string `json:"holder"`
	// StartedBy is the actor who started it.
	StartedBy string `json:"startedBy"`
	// StartedAt is when it was started.
	StartedAt time.Time `json:"startedAt"`
	// Heartbeat is when the holder last said it was alive.
	Heartbeat time.Time `json:"heartbeat"`
}

// resultRecord is the value of ResultAnnotation.
type resultRecord struct {
	Run       string        `json:"run"`
	Target    string        `json:"target"`
	Image     string        `json:"image"`
	Holder    string        `json:"holder"`
	StartedBy string        `json:"startedBy"`
	StartedAt time.Time     `json:"startedAt"`
	EndedAt   time.Time     `json:"endedAt"`
	State     release.State `json:"state"`
	// Node, NodeState and Reason say which node stopped the run, where it
	// was left, and why; empty when the run was done.
	Node      string            `json:"node,omitempty"`
	NodeState release.NodeState `json:"nodeState,omitempty"`
	Reason    string            `json:"reason,omitempty"`
}

// previousRecord is the value of PreviousAnnotation.
type previousRecord struct {
	Image   string `json:"image"`
	Version string `json:"version,omitempty"`
}

// maxRecord bounds an annotation that is read.
const maxRecord = 4096

// parseLock reads a lock annotation; ok is false when there is none, and
// err not nil when there is one that does not parse - which is treated as
// a lock whose holder is gone.
func parseLock(annotations map[string]string) (lock lockRecord, ok bool, err error) {
	text, ok := annotations[LockAnnotation]
	if !ok {
		return lockRecord{}, false, nil
	}
	if len(text) > maxRecord {
		return lockRecord{}, true, errors.New("the lock annotation is too large")
	}
	if err := json.Unmarshal([]byte(text), &lock); err != nil {
		return lockRecord{}, true, err
	}
	if lock.Run == "" || lock.Image == "" {
		return lockRecord{}, true, errors.New("the lock annotation names no run")
	}
	if _, err := release.ParseVersion(lock.Target); err != nil {
		return lockRecord{}, true, err
	}
	return lock, true, nil
}

// parseResult reads a result annotation; ok is false when there is none or
// it does not parse.
func parseResult(annotations map[string]string) (result resultRecord, ok bool) {
	text, found := annotations[ResultAnnotation]
	if !found || len(text) > maxRecord {
		return resultRecord{}, false
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil || !result.State.Ended() {
		return resultRecord{}, false
	}
	if _, err := release.ParseVersion(result.Target); err != nil {
		return resultRecord{}, false
	}
	return result, true
}

// parsePrevious reads a previous annotation; the zero record when there is
// none or it does not parse.
func parsePrevious(annotations map[string]string) previousRecord {
	var prev previousRecord
	text, ok := annotations[PreviousAnnotation]
	if !ok || len(text) > maxRecord || json.Unmarshal([]byte(text), &prev) != nil {
		return previousRecord{}
	}
	return prev
}

// encode returns v as the text of an annotation.
func encode(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		// The records are structs of strings and times: they always
		// encode.
		return "{}"
	}
	return string(data)
}

// digestPattern is a digest of a manifest list.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// repositoryOf returns the repository of an image reference, without its
// tag or digest: "registry:5000/cactusfs:0.7.0" is "registry:5000/cactusfs".
func repositoryOf(image string) string {
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	slash := strings.LastIndexByte(image, '/')
	if colon := strings.LastIndexByte(image, ':'); colon > slash {
		image = image[:colon]
	}
	return image
}

// labelValue is a value a label may hold.
var labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)

// versionLabel returns the version as the value of VersionLabel, or ""
// when it cannot be one.
func versionLabel(v release.Version) string {
	s := v.String()
	if len(s) > 63 || !labelValue.MatchString(s) {
		return ""
	}
	return s
}
