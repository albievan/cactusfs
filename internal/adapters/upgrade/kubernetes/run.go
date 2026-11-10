package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/release"
)

// releaseAttempts is how often the lock is asked to be released before the
// run gives up and leaves it to be taken over.
const releaseAttempts = 5

// errLost is a run whose lock was taken by another: it stops, and touches
// nothing more.
var errLost = errors.New("another node took the lock of the run")

// errSettled is a run whose lock another node replaced with the run's own
// result "done", having seen every node at the target: it is over, and
// touches nothing more.
var errSettled = errors.New("another node saw the run done")

// nodeFailure is why a node stopped the run, and where it was left.
type nodeFailure struct {
	state  release.NodeState
	reason string
}

// Error implements error.
func (f *nodeFailure) Error() string { return f.reason }

// run is one upgrade under way in this process. Only its goroutine touches
// it once Start has returned.
type run struct {
	u        *Upgrader
	ns       string
	lockNode string
	order    []string
	lock     lockRecord
	target   release.Version
	label    string
	report   func(release.Progress)
	// beat is when the heartbeat was last written.
	beat     time.Time
	progress release.Progress
}

// emit reports the progress, a copy of it.
func (r *run) emit() {
	if r.report != nil {
		r.report(r.progress.Normalized())
	}
}

// setNode records where node i stands, and reports it.
func (r *run) setNode(i int, state release.NodeState, reason string, build *cluster.Build) {
	n := &r.progress.Nodes[i]
	n.State, n.Reason = state, reason
	if build != nil {
		n.Version, n.Build = build.Version, build.Build
	}
	r.emit()
}

// execute moves every node in order and ends the run.
func (r *run) execute(ctx context.Context) {
	defer r.u.stopped()
	r.emit()
	for i, node := range r.order {
		err := r.move(ctx, i, node)
		var failed *nodeFailure
		switch {
		case err == nil:
			continue
		case errors.Is(err, errSettled):
			r.u.log.InfoContext(ctx, "upgrade done, as another node saw it", "run", r.lock.Run, "target", r.lock.Target)
			r.progress.State = release.StateDone
			for j := i; j < len(r.order); j++ {
				r.progress.Nodes[j].State, r.progress.Nodes[j].Reason = release.NodeDone, ""
			}
			r.emit()
			return
		case errors.Is(err, errLost):
			r.u.log.WarnContext(ctx, "upgrade stopped: another node took its lock", "run", r.lock.Run, "node", node)
			r.progress.State = release.StateFailed
			r.setNode(i, release.NodeFailed, "the run stopped: another node took it over", nil)
			return
		case errors.As(err, &failed):
			r.u.log.WarnContext(ctx, "upgrade stopped at a node", "run", r.lock.Run, "node", node, "state", string(failed.state), "reason", failed.reason)
			r.finish(ctx, release.StateFailed, node, failed)
			r.progress.State = release.StateFailed
			r.setNode(i, failed.state, failed.reason, nil)
			return
		}
	}
	r.u.log.InfoContext(ctx, "upgrade done", "run", r.lock.Run, "target", r.lock.Target)
	r.finish(ctx, release.StateDone, "", nil)
	r.progress.State = release.StateDone
	r.emit()
}

// move moves node i to the image of the run: nil when it runs it and is
// ready, errLost, or a *nodeFailure once it has been put back where that
// could be done.
func (r *run) move(ctx context.Context, i int, node string) error {
	r.setNode(i, release.NodeUpgrading, "", nil)
	nodeCtx, cancel := context.WithTimeout(ctx, r.u.cfg.NodeTimeout)
	defer cancel()
	name := r.u.cfg.Container
	sts, err := r.u.api.getStatefulSet(nodeCtx, r.ns, node)
	if err != nil {
		return &nodeFailure{release.NodeFailed, "its StatefulSet could not be read: " + describe(err)}
	}
	current := sts.Spec.Template.Spec.image(name)
	if current == "" {
		return &nodeFailure{release.NodeFailed, "its StatefulSet has no container " + name}
	}
	p, err := r.u.api.getPod(nodeCtx, r.ns, node+"-0")
	if err != nil {
		return &nodeFailure{release.NodeFailed, "its pod could not be read: " + describe(err)}
	}
	if current == r.lock.Image && p.readyWith(name, r.lock.Image) {
		if build, ok := r.reports(nodeCtx, node); ok {
			// At the target already: moved by a run that was cut off,
			// or by hand.
			r.setNode(i, release.NodeDone, "", &build)
			return nil
		}
	}
	prev := parsePrevious(sts.Metadata.Annotations)
	if current != r.lock.Image {
		prev = previousRecord{Image: current, Version: sts.Spec.Template.Metadata.Labels[VersionLabel]}
		if err := r.patchImage(nodeCtx, node, r.lock.Image, r.label, prev); err != nil {
			return &nodeFailure{release.NodeFailed, "its StatefulSet could not be patched: " + describe(err)}
		}
	}
	if p != nil && p.Metadata.DeletionTimestamp == nil && p.Spec.image(name) != r.lock.Image {
		if err := r.evict(nodeCtx, node, true); err != nil {
			if errors.Is(err, errLost) || errors.Is(err, errSettled) {
				return err
			}
			return r.rollback(ctx, node, prev, err.Error())
		}
	}
	build, err := r.wait(nodeCtx, node)
	if err != nil {
		if errors.Is(err, errLost) || errors.Is(err, errSettled) {
			return err
		}
		return r.rollback(ctx, node, prev, err.Error())
	}
	r.u.log.InfoContext(ctx, "upgrade: node moved", "run", r.lock.Run, "node", node, "target", r.lock.Target)
	r.setNode(i, release.NodeDone, "", &build)
	return nil
}

// reports asks a node for its build: ok when it reports the target.
func (r *run) reports(ctx context.Context, node string) (cluster.Build, bool) {
	build, err := r.u.cfg.NodeBuild(ctx, node)
	return build, err == nil && isVersion(build.Version, r.target)
}

// patchImage patches a node's StatefulSet to image and the version label,
// keeping what it ran before in PreviousAnnotation; a strategic merge patch,
// so that the container is matched by its name.
func (r *run) patchImage(ctx context.Context, node, image, label string, prev previousRecord) error {
	return r.u.api.patchStatefulSet(ctx, r.ns, node, strategicPatch, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{PreviousAnnotation: encode(prev)}},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{VersionLabel: label}},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": r.u.cfg.Container, "image": image},
			}},
		}},
	})
}

// evict evicts a node's pod, asking again while the PodDisruptionBudget
// says not now (429), until ctx ends. With beat, the heartbeat is kept
// alive meanwhile, and a lock taken by another stops it with errLost.
func (r *run) evict(ctx context.Context, node string, beat bool) error {
	budget := fmt.Errorf("the PodDisruptionBudget did not let its pod be evicted within %s", r.u.cfg.NodeTimeout)
	refused := false
	for {
		if beat {
			if err := r.tick(ctx); err != nil {
				// A lock taken by another run is never the budget's doing:
				// the caller must stop and touch nothing more.
				if !errors.Is(err, errLost) && !errors.Is(err, errSettled) && refused && ctx.Err() != nil {
					return budget
				}
				return err
			}
		}
		err := r.u.api.evict(ctx, r.ns, node+"-0")
		if err == nil {
			return nil
		}
		// Once the budget has said not now, a request the node's time
		// cut short is the budget's doing, not an API out of reach.
		if refused && ctx.Err() != nil {
			return budget
		}
		if statusOf(err) != http.StatusTooManyRequests {
			return fmt.Errorf("its pod could not be evicted: %s", describe(err))
		}
		refused = true
		select {
		case <-ctx.Done():
			return budget
		case <-time.After(r.u.cfg.Poll):
		}
	}
}

// wait waits until a node's pod is Ready on the image of the run, the node
// reports the target, and the cluster is ready; until ctx ends.
func (r *run) wait(ctx context.Context, node string) (cluster.Build, error) {
	name := r.u.cfg.Container
	why := "its pod did not become ready on the new image"
	for {
		if err := r.tick(ctx); err != nil {
			return cluster.Build{}, err
		}
		p, err := r.u.api.getPod(ctx, r.ns, node+"-0")
		if err == nil && p.readyWith(name, r.lock.Image) {
			why = "the node did not report the version " + r.lock.Target
			if build, ok := r.reports(ctx, node); ok {
				why = "the cluster did not become ready"
				if r.u.cfg.Ready(ctx) == nil {
					return build, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return cluster.Build{}, fmt.Errorf("%s within %s", why, r.u.cfg.NodeTimeout)
		case <-time.After(r.u.cfg.Poll):
		}
	}
}

// rollback puts a node that did not come up back to what it ran before and
// evicts its pod again, and returns why the run stops. It first reads the
// lock again, under the run's context and not the node's - which may be
// what ran out: a run whose lock another took (errLost) or settled
// (errSettled) puts nothing back and evicts nothing, for the node is then
// the other run's.
func (r *run) rollback(ctx context.Context, node string, prev previousRecord, reason string) error {
	if err := r.holds(ctx); err != nil {
		return err
	}
	if prev.Image == "" {
		return &nodeFailure{release.NodeFailed, reason + "; what it ran before is not known, so it was not put back"}
	}
	backCtx, cancel := context.WithTimeout(ctx, r.u.cfg.NodeTimeout)
	defer cancel()
	var label any
	if prev.Version != "" {
		label = prev.Version
	}
	err := r.u.api.patchStatefulSet(backCtx, r.ns, node, strategicPatch, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{PreviousAnnotation: nil}},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{VersionLabel: label}},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": r.u.cfg.Container, "image": prev.Image},
			}},
		}},
	})
	if err != nil {
		return &nodeFailure{release.NodeFailed, reason + "; it could not be put back: " + describe(err)}
	}
	if err := r.evict(backCtx, node, false); err != nil {
		return &nodeFailure{release.NodeFailed, reason + "; its StatefulSet was put back, and " + err.Error()}
	}
	return &nodeFailure{release.NodeRestored, reason + "; it was put back to the image it ran"}
}

// holds reads the lock now, whatever the heartbeat's time: errLost or
// errSettled when it is no longer this run's, nil when it is or cannot be
// read - a read that fails is not a lock lost, as at the heartbeat.
func (r *run) holds(ctx context.Context) error {
	readCtx, cancel := context.WithTimeout(ctx, 2*requestTimeout)
	defer cancel()
	sts, err := r.u.api.getStatefulSet(readCtx, r.ns, r.lockNode)
	if err != nil {
		r.u.log.WarnContext(ctx, "upgrade: the lock could not be read before a node is put back", "run", r.lock.Run, "error", describe(err))
		return nil
	}
	return r.lost(sts.Metadata.Annotations)
}

// lost says whether annotations still hold this run's lock: nil when they
// do, errSettled when the lock was replaced by this run's result "done",
// errLost otherwise.
func (r *run) lost(annotations map[string]string) error {
	held, found, perr := parseLock(annotations)
	if found && perr == nil && held.Run == r.lock.Run {
		return nil
	}
	if !found {
		if result, ok := parseResult(annotations); ok && result.Run == r.lock.Run && result.State == release.StateDone {
			return errSettled
		}
	}
	return errLost
}

// heartbeatEvery is how often the lock says its holder is alive: a quarter
// of a node's time, and not more often than a node is looked at.
func (r *run) heartbeatEvery() time.Duration {
	return max(r.u.cfg.NodeTimeout/4, r.u.cfg.Poll)
}

// tick writes the heartbeat when it is due: errLost when the lock is no
// longer this run's, errSettled when another node replaced it with this
// run's result "done". A heartbeat that cannot be written is tried again at
// the next tick; the lock is taken over only once it is twice a node's
// time old.
func (r *run) tick(ctx context.Context) error {
	now := r.u.cfg.Clock.Now().UTC()
	if now.Sub(r.beat) < r.heartbeatEvery() {
		return nil
	}
	sts, err := r.u.api.getStatefulSet(ctx, r.ns, r.lockNode)
	if err != nil {
		r.u.log.WarnContext(ctx, "upgrade: the heartbeat could not be read", "run", r.lock.Run, "error", describe(err))
		return nil
	}
	if err := r.lost(sts.Metadata.Annotations); err != nil {
		return err
	}
	lock := r.lock
	lock.Heartbeat = now
	err = r.u.api.patchStatefulSet(ctx, r.ns, r.lockNode, mergePatch, map[string]any{
		"metadata": map[string]any{
			"resourceVersion": sts.Metadata.ResourceVersion,
			"annotations":     map[string]any{LockAnnotation: encode(lock)},
		},
	})
	if err != nil {
		r.u.log.WarnContext(ctx, "upgrade: the heartbeat could not be written", "run", r.lock.Run, "error", describe(err))
		return nil
	}
	r.lock, r.beat = lock, now
	return nil
}

// finish releases the lock and writes how the run ended in its place, in
// one patch under the StatefulSet's resourceVersion; a lock that is no
// longer this run's is left alone.
func (r *run) finish(ctx context.Context, state release.State, node string, failed *nodeFailure) {
	result := resultRecord{
		Run: r.lock.Run, Target: r.lock.Target, Image: r.lock.Image, Holder: r.lock.Holder,
		StartedBy: r.lock.StartedBy, StartedAt: r.lock.StartedAt, EndedAt: r.u.cfg.Clock.Now().UTC(),
		State: state, Node: node,
	}
	if failed != nil {
		result.NodeState, result.Reason = failed.state, failed.reason
	}
	for attempt := range releaseAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.u.cfg.Poll):
			}
		}
		endCtx, cancel := context.WithTimeout(ctx, 2*requestTimeout)
		done := r.release(endCtx, result)
		cancel()
		if done {
			return
		}
	}
	r.u.log.WarnContext(ctx, "upgrade: the lock could not be released; it will be taken over", "run", r.lock.Run)
}

// release makes one attempt at finish: true when it is over, one way or
// the other.
func (r *run) release(ctx context.Context, result resultRecord) bool {
	sts, err := r.u.api.getStatefulSet(ctx, r.ns, r.lockNode)
	if err != nil {
		return false
	}
	if held, found, perr := parseLock(sts.Metadata.Annotations); !found || perr != nil || held.Run != r.lock.Run {
		return true
	}
	err = r.u.api.patchStatefulSet(ctx, r.ns, r.lockNode, mergePatch, map[string]any{
		"metadata": map[string]any{
			"resourceVersion": sts.Metadata.ResourceVersion,
			"annotations":     map[string]any{LockAnnotation: nil, ResultAnnotation: encode(result)},
		},
	})
	return err == nil
}
