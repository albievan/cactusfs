package iam

import "slices"

// What a policy does, statement by statement (ADR-0032): which actions each
// covers, whether it is open to anyone, and what of that a caller without
// credentials could really be let in to. The analysis classifies; it
// decides nothing. Conditions, resources and the other statements are not
// weighed - what a caller may do is Decide's to say.

// StatementAnalysis is what one statement of a policy does.
type StatementAnalysis struct {
	// Index is the statement's index in the document, from 0.
	Index int
	// Sid is the statement's Sid; empty when it has none.
	Sid string
	// Effect is the statement's effect.
	Effect Effect
	// Actions are the actions of the catalogue its action patterns cover,
	// in the order of the catalogue. An action that no wildcard grants
	// (s3:BypassGovernanceRetention, s3:ReplicateObject,
	// s3:ReplicateDelete) is covered by an Allow only when the Allow names
	// it; a Deny covers it by a wildcard as well.
	Actions []Action
	// OpenToAnyone says that the statement is an Allow whose principals
	// include "*": of a bucket's or a sub-bucket's policy only, and never
	// of a sub-bucket's, whose parser refuses one.
	OpenToAnyone bool
	// AnonymousActions are those of Actions that a caller without
	// credentials could be let in to by the statement, before its
	// conditions: empty but in an open statement of a bucket's policy.
	AnonymousActions []Action
	// NeverAnonymous are those of Actions that an open statement names for
	// anyone and that are never allowed without credentials: such a
	// statement allows them to every user, and to nobody unauthenticated.
	NeverAnonymous []Action
	// UnmatchedPatterns are the statement's action patterns, as written,
	// that cover no action of the catalogue: a misspelling, an action of
	// another release, or a wildcard of an Allow that would grant only an
	// action no wildcard grants. The parser takes them; this is where they
	// show.
	UnmatchedPatterns []string
}

// AnalysePolicy says what each statement of an identity policy - a named
// policy, or the inline policy of an API key - does. No statement of one is
// open to anyone.
func AnalysePolicy(doc Document) []StatementAnalysis {
	analyses := make([]StatementAnalysis, len(doc.Statements))
	for i, s := range doc.Statements {
		analyses[i] = analyseStatement(i, s)
	}
	return analyses
}

// AnalyseBucketPolicy says what each statement of a bucket's policy does,
// for whom it is open as well.
func AnalyseBucketPolicy(p BucketPolicy) []StatementAnalysis {
	analyses := make([]StatementAnalysis, len(p.Document.Statements))
	for i, s := range p.Document.Statements {
		analyses[i] = analyseStatement(i, s)
		if s.Effect != Allow || i >= len(p.Principals) || !slices.Contains(p.Principals[i], PrincipalAnyone) {
			continue
		}
		analyses[i].OpenToAnyone = true
		for _, action := range analyses[i].Actions {
			if anonymousReadable(action) {
				analyses[i].AnonymousActions = append(analyses[i].AnonymousActions, action)
			} else {
				analyses[i].NeverAnonymous = append(analyses[i].NeverAnonymous, action)
			}
		}
	}
	return analyses
}

// AnalyseSubBucketPolicy says what each statement of a sub-bucket's policy
// does. A sub-bucket's policy lets no caller without credentials in (Decide
// reads only its Deny for one), so none of its statements is open.
func AnalyseSubBucketPolicy(p SubBucketPolicy) []StatementAnalysis {
	analyses := AnalysePolicy(p.policy.Document)
	for i := range analyses {
		analyses[i].OpenToAnyone = false
		analyses[i].AnonymousActions = nil
		analyses[i].NeverAnonymous = nil
	}
	return analyses
}

// analyseStatement says which actions a statement covers, and which of its
// patterns cover none.
func analyseStatement(i int, s Statement) StatementAnalysis {
	analysis := StatementAnalysis{Index: i, Sid: s.Sid, Effect: s.Effect}
	actions := analysedActions()
	for _, action := range actions {
		if slices.ContainsFunc(s.Actions, func(pattern string) bool { return patternCovers(s.Effect, pattern, action) }) {
			analysis.Actions = append(analysis.Actions, action)
		}
	}
	for _, pattern := range s.Actions {
		if !slices.ContainsFunc(actions, func(action Action) bool { return patternCovers(s.Effect, pattern, action) }) {
			analysis.UnmatchedPatterns = append(analysis.UnmatchedPatterns, pattern)
		}
	}
	return analysis
}

// analysedActions are the actions a policy can grant - every s3: and admin:
// action of the catalogue (Catalogue), in its order - that the analysis says
// a statement covers. No cluster: action is among them: no policy grants
// one. TestAnalysedActionsAreTheCatalogue holds the two alike.
func analysedActions() []Action {
	catalogue := Catalogue()
	actions := make([]Action, len(catalogue))
	for i, entry := range catalogue {
		actions[i] = entry.Name
	}
	return actions
}
