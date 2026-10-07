// Package cluster holds the domain model for cluster membership and node health.
package cluster

// CheckResult is the outcome of a single readiness check.
type CheckResult struct {
	// Name identifies the check.
	Name string
	// Err is nil when the check passed.
	Err error
}

// OK reports whether the check passed.
func (c CheckResult) OK() bool {
	return c.Err == nil
}

// Readiness is the aggregated outcome of every readiness check of a node.
type Readiness struct {
	// Checks holds one result per check, in the order the checks ran.
	Checks []CheckResult
}

// Ready reports whether every check passed. A node without checks is ready.
func (r Readiness) Ready() bool {
	for _, c := range r.Checks {
		if !c.OK() {
			return false
		}
	}
	return true
}

// Failed returns the checks that did not pass, in the order they ran.
func (r Readiness) Failed() []CheckResult {
	var failed []CheckResult
	for _, c := range r.Checks {
		if !c.OK() {
			failed = append(failed, c)
		}
	}
	return failed
}
