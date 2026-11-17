package iam

import (
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
)

// The decision as it was before Explain was made (commit bd35f29), frozen:
// Decide, Evaluate, Statement.matches, judge, BucketPolicy.judge and
// SubBucketPolicy.judge, copied unchanged but for their names (old*).
// FuzzExplainAgreesWithDecide and TestDecideAgreesWithTheOracle hold the
// decision to them - the same source, the same error text, the same
// sentinels - and TestDecideAllocations holds Decide to their allocations,
// measured in the same run. They use the helpers of the package that did
// not change (Condition.holds, patternCovers, wildcardMatch,
// anonymousReadable, serviceMay, owns, SubBucketPolicy.covers). Nothing but
// those tests calls them.

// From iam.go of bd35f29.
func oldDecide(req Request) error {
	_, err := oldEvaluate(req)
	return err
}

// From iam.go of bd35f29.
func oldEvaluate(req Request) (Source, error) {
	if req.Principal.LockedSession && req.Action != ActionPutObject {
		// First of all: no policy, no kind of principal and no kind of
		// action has a say in it.
		return SourceRule, fmt.Errorf("%w: %s may not %s on %s until it is unlocked", domain.ErrSessionLocked, req.Principal.Name(), req.Action, req.Resource)
	}
	if req.Principal.Anonymous() {
		// Only the policy of a bucket lets anybody in unauthenticated,
		// and only to read. The policy of a sub-bucket allows nobody
		// anonymous, but its Deny holds for them as for anybody: a
		// private sub-bucket of a public bucket stays private.
		session := Context{SourceIP: req.Principal.Session.SourceIP, SecureTransport: req.Principal.Session.SecureTransport, Now: req.Now}
		if req.SubBucketPolicy != nil && req.SubBucketPolicy.oldJudge(req, req.Resource.String(), session) == denied {
			return SourceSubBucket, fmt.Errorf("%w: anonymous principal may not %s on %s", domain.ErrAccessDenied, req.Action, req.Resource)
		}
		source := SourceRule
		if req.BucketPolicy != nil && anonymousReadable(req.Action) {
			switch req.BucketPolicy.oldJudge(req.Principal, req.Action, req.Resource.String(), session) {
			case allowed:
				return SourceBucket, nil
			case denied:
				source = SourceBucket
			default:
				source = SourceNone
			}
		}
		return source, fmt.Errorf("%w: anonymous principal may not %s on %s", domain.ErrAccessDenied, req.Action, req.Resource)
	}
	refused := fmt.Errorf("%w: %s may not %s on %s", domain.ErrAccessDenied, req.Principal.Name(), req.Action, req.Resource)
	if service := req.Principal.Service; service != "" {
		// A service is that and nothing else, and is bound by no policy:
		// what it may do is fixed here.
		p := req.Principal
		if p.AccessKeyID == "" && !p.Root && p.NodeID == "" && p.User == "" && serviceMay(service, req.Action) {
			return SourceRule, nil
		}
		return SourceRule, refused
	}
	if node := req.Principal.NodeID; node != "" || req.Action.cluster() {
		// A node adds, records and removes itself only, and reports no
		// counts but its own. A node that was taken out of the cluster
		// does none of it, whatever certificate it shows.
		own := req.Action == ActionNodeRPC || req.Resource.Node == node
		if node != "" && !req.Principal.NodeRevoked && req.Principal.AccessKeyID == "" && !req.Principal.Root &&
			req.Principal.User == "" && req.Action.cluster() && own {
			return SourceRule, nil
		}
		return SourceRule, refused
	}
	if req.Principal.Root {
		if req.Principal.User != "" {
			return SourceRule, refused
		}
		return SourceRule, nil
	}
	if req.Principal.User == "" {
		return SourceRule, refused
	}
	ctx := Context{
		SourceIP: req.Principal.Session.SourceIP, SecureTransport: req.Principal.Session.SecureTransport,
		Now: req.Now, Username: req.Principal.User,
	}
	resource := req.Resource.String()
	// The user's own policies, the bucket's and the sub-bucket's: any may
	// allow, and a Deny in any wins over every Allow. The sub-bucket's has
	// a say only in what it covers.
	verdicts := []struct {
		source  Source
		verdict verdict
	}{{SourceIdentity, oldJudge(req.Principal.Policies, req.Action, resource, ctx)}, {SourceBucket, silent}, {SourceSubBucket, silent}}
	if req.BucketPolicy != nil {
		verdicts[1].verdict = req.BucketPolicy.oldJudge(req.Principal, req.Action, resource, ctx)
	}
	if req.SubBucketPolicy != nil {
		verdicts[2].verdict = req.SubBucketPolicy.oldJudge(req, resource, ctx)
	}
	for _, v := range verdicts {
		if v.verdict == denied {
			return v.source, refused
		}
	}
	source := SourceNone
	for _, v := range verdicts {
		if v.verdict == allowed {
			source = v.source
			break
		}
	}
	if source == SourceNone && owns(req) {
		source = SourceOwner
	}
	if source == SourceNone {
		return SourceNone, refused
	}
	if boundary := req.Principal.Boundary; boundary != nil && oldJudge([]Document{*boundary}, req.Action, resource, ctx) != allowed {
		return SourceKeyBound, fmt.Errorf("%w: the API key's own policy does not allow %s on %s", domain.ErrAccessDenied, req.Action, req.Resource)
	}
	return source, nil
}

// From policy.go of bd35f29.
func (s Statement) oldMatches(action Action, resource string, ctx Context) bool {
	hit := false
	for _, pattern := range s.Actions {
		if patternCovers(s.Effect, pattern, action) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	hit = false
	for _, pattern := range s.Resources {
		if pattern == "*" || wildcardMatch(pattern, resource) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, c := range s.Conditions {
		if !c.holds(ctx, s.Effect) {
			return false
		}
	}
	return true
}

// From policy.go of bd35f29.
func oldJudge(documents []Document, action Action, resource string, ctx Context) verdict {
	result := silent
	for _, doc := range documents {
		for _, s := range doc.Statements {
			if !s.oldMatches(action, resource, ctx) {
				continue
			}
			if s.Effect == Deny {
				return denied
			}
			result = allowed
		}
	}
	return result
}

// From bucketpolicy.go of bd35f29.
func (p BucketPolicy) oldJudge(principal Principal, action Action, resource string, ctx Context) verdict {
	result := silent
	for i, s := range p.Document.Statements {
		about := false
		for _, name := range p.Principals[i] {
			// A user is named by name. The root identity and anonymous
			// callers have none, and are reached by "*" only.
			about = about || name == PrincipalAnyone || (principal.User != "" && name == principal.User)
		}
		if !about || !s.oldMatches(action, resource, ctx) {
			continue
		}
		if s.Effect == Deny {
			return denied
		}
		// What only a user's own policy gives, by name, no bucket policy
		// gives - not even one that names it, should one have been stored
		// before ParseBucketPolicy refused it.
		if action.byNameOnly() {
			continue
		}
		result = allowed
	}
	return result
}

// From subbucketpolicy.go of bd35f29.
func (p SubBucketPolicy) oldJudge(req Request, resource string, ctx Context) verdict {
	if !p.covers(req) {
		return silent
	}
	return p.policy.oldJudge(req.Principal, req.Action, resource, ctx)
}
