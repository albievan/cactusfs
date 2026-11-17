package iam

// Rule names the rule of Decide that answered a request no policy had a say
// in (SourceRule).
type Rule string

// The rules of a decision.
const (
	// RuleRoot is the root identity, which may do everything a user could
	// be allowed and is bound by no policy.
	RuleRoot Rule = "root"
	// RuleAnonymous is a caller who has not authenticated, asking what no
	// bucket policy can open to one: there was no bucket policy, or the
	// action is never anonymous.
	RuleAnonymous Rule = "anonymous"
	// RuleService is a service of the node, whose actions are fixed.
	RuleService Rule = "service"
	// RuleNode is a node of the cluster, or a cluster action, which belong
	// to each other and to nothing else.
	RuleNode Rule = "node"
	// RuleLockedSession is a locked console session, which may store
	// objects and nothing else (ADR-0027).
	RuleLockedSession Rule = "locked_session"
	// RuleNoUser is a principal that is none of the others and names no
	// user: it has no policy, so nothing allows it anything.
	RuleNoUser Rule = "no_user"
)

// CitedStatement names one statement of a policy that had a say in a
// decision.
type CitedStatement struct {
	// Source is where the statement is: the caller's own policies, the
	// bucket's, the sub-bucket's, or the API key's inline policy.
	Source Source
	// Policy is the name of the policy the statement is in, for one of
	// the caller's own (SourceIdentity) when the principal names its
	// policies (Principal.PolicyNames); empty otherwise.
	Policy string
	// Document is the index of the document in its source, from 0: of
	// Principal.Policies for SourceIdentity, and 0 for every other
	// source, which has one document.
	Document int
	// Statement is the index of the statement in its document, from 0.
	Statement int
	// Sid is the statement's Sid, or empty.
	Sid string
	// Effect is the statement's effect.
	Effect Effect
}

// KeyPolicyVerdict is what the inline policy of an API key said of a request.
type KeyPolicyVerdict struct {
	// Allowed reports whether the key's own policy allowed the request.
	// It is told whatever the other sources said; it decides only when
	// one of them allowed.
	Allowed bool
	// Statements are the statements of the key's policy that matched.
	Statements []CitedStatement
}

// Explanation is a decision and why it was made: the answer of Decide and
// Evaluate, and what they read to give it.
type Explanation struct {
	// Allowed reports whether the request is allowed: Decide returns nil.
	Allowed bool
	// Source is the source of the answer, as Evaluate names it.
	Source Source
	// Rule is the rule that answered when Source is SourceRule, and empty
	// otherwise.
	Rule Rule
	// Statements are the statements that matched, of every source that
	// had a say, in the order they were read: the caller's own policies,
	// the bucket's, the sub-bucket's (for a caller who has not
	// authenticated, the sub-bucket's before the bucket's). A statement
	// about another principal, of a sub-bucket policy that does not cover
	// the request, or of a source no rule lets have a say, is not among
	// them; neither is an Allow of a bucket's or a sub-bucket's policy of
	// an action only a user's own policy gives.
	Statements []CitedStatement
	// KeyPolicy is what the API key's inline policy said, when the
	// principal has one and the decision came to the policies; nil
	// otherwise.
	KeyPolicy *KeyPolicyVerdict
	// UnknownAddress are the statements, matched or not, one of whose
	// conditions on the source address was decided without an address:
	// against the request, whichever way that cuts (Condition.holds).
	UnknownAddress []CitedStatement

	err error
}

// Err returns the refusal, exactly as Evaluate and Decide return it: nil
// when the request is allowed; otherwise an error wrapping
// domain.ErrAccessDenied, or domain.ErrSessionLocked for a locked session.
func (e Explanation) Err() error { return e.err }

// Explain answers an authorization request as Decide does, and says why: the
// source and rule of the answer, the statements of each source that matched,
// what the key's own policy said, and which conditions on the source address
// were decided without one. It is the one evaluation of Decide and Evaluate,
// which are its projections: its Allowed, Source and Err are always theirs.
func Explain(req Request) Explanation {
	var c collector
	source, rule, err := evaluate(req, &c)
	return Explanation{
		Allowed: err == nil, Source: source, Rule: rule,
		Statements: c.statements, KeyPolicy: c.key, UnknownAddress: c.unknown,
		err: err,
	}
}

// collector gathers, for Explain, what the judges read. Decide and Evaluate
// pass none.
type collector struct {
	// names are the names of the caller's own policies, when there is one
	// for each document.
	names      []string
	statements []CitedStatement
	key        *KeyPolicyVerdict
	unknown    []CitedStatement
}

// name takes the names of the principal's policies, when it names each one.
func (c *collector) name(p Principal) {
	if len(p.PolicyNames) == len(p.Policies) {
		c.names = p.PolicyNames
	}
}

// cite records statement i of document d of source: among the statements
// that matched when matched, among those decided on an unknown address when
// unknown. A statement of the key's own policy that matched goes to the key's
// verdict, made here if it was not yet.
func (c *collector) cite(source Source, d, i int, s *Statement, matched, unknown bool) {
	cited := CitedStatement{Source: source, Document: d, Statement: i, Sid: s.Sid, Effect: s.Effect}
	if source == SourceIdentity && d < len(c.names) {
		cited.Policy = c.names[d]
	}
	if unknown {
		c.unknown = append(c.unknown, cited)
	}
	switch {
	case !matched:
	case source == SourceKeyBound:
		if c.key == nil {
			c.key = &KeyPolicyVerdict{}
		}
		c.key.Statements = append(c.key.Statements, cited)
	default:
		c.statements = append(c.statements, cited)
	}
}
