package iam

import (
	"slices"
	"strings"
)

// The catalogue of the policy language (ADR-0032): what a policy can say, as
// this binary understands it. It is what the policy builder offers, and what
// the management API answers at /policy-catalogue. It is a property of the
// binary, not of the state: two nodes of different releases may answer two
// catalogues, and each judges a policy by its own.
//
// It describes and decides nothing. Every flag of an action is the predicate
// Decide and the parsers use, read here and not restated; a test ties every
// action constant of this package to the catalogue, and every flag to the
// behaviour of Decide and of the three parsers.

// CatalogueAction describes one action a policy can grant.
type CatalogueAction struct {
	// Name is the action as a policy names it: "s3:GetObject".
	Name Action
	// Group is the part of the product the action belongs to, as a title:
	// "Objects", "Users". Groups are only ever added.
	Group string
	// Description says, in a sentence, what the action allows.
	Description string
	// Resources are the templates of the resources the action is asked on,
	// such as "arn:aws:s3:::{bucket}/{key}" or
	// "arn:cactusfs:admin:::user/{name}"; each is one of ResourceKinds.
	Resources []string
	// Read says that the action changes no object and no configuration:
	// it reads. Its reads may still be counted (ADR-0016) and recorded in
	// the audit trail.
	Read bool
	// ByNameOnly says that no wildcard of an Allow grants the action: a
	// statement allows it only by naming it, and no bucket policy allows it
	// at all. A Deny covers it however it is written.
	ByNameOnly bool
	// InBucketPolicy says that a bucket policy may allow the action.
	InBucketPolicy bool
	// Anonymous says that a bucket policy can open the action to a caller
	// who has not authenticated.
	Anonymous bool
	// InSubBucketPolicy says that the policy of a sub-bucket may name the
	// action, by its full name: no wildcard there covers it. A listing it
	// may name in an Allow only.
	InSubBucketPolicy bool
}

// The groups of the catalogue.
const (
	groupBuckets      = "Buckets"
	groupObjects      = "Objects"
	groupVersions     = "Versions"
	groupObjectLock   = "Object Lock"
	groupMultipart    = "Multipart uploads"
	groupBucketConfig = "Bucket configuration"
	groupSubBuckets   = "Sub-buckets"
	groupReplication  = "Replication"
	groupUsers        = "Users"
	groupGroups       = "Groups"
	groupRoles        = "Roles"
	groupPolicies     = "Policies"
	groupAPIKeys      = "API keys"
	groupAudit        = "Audit"
	groupCluster      = "Cluster"
	groupQuotas       = "Quotas"
	groupTargets      = "Targets"
	groupTagCatalogue = "Tag catalogue"
	groupUpdates      = "Releases"
)

// The templates of the resources actions are asked on. A placeholder in
// braces stands for a name: {bucket} for a bucket's, {key} for an object's
// key, {tag_key} for the key of a tag of the catalogue of tags, {name} for
// the name of any other administrative resource. Outside the placeholders
// a template is literal, so a resource fits it when the placeholders, each
// replaced by one or more characters, make it the resource's ARN
// (Resource.String): what each placeholder may hold is FitsTemplate's.
const (
	resourceAnyPattern     = "*"
	templateAllBuckets     = s3ARNPrefix + "*"
	templateBucket         = s3ARNPrefix + "{bucket}"
	templateObject         = s3ARNPrefix + "{bucket}/{key}"
	templateUsers          = adminARNPrefix + "user"
	templateUser           = adminARNPrefix + "user/{name}"
	templateGroups         = adminARNPrefix + "group"
	templateGroup          = adminARNPrefix + "group/{name}"
	templateRoles          = adminARNPrefix + "role"
	templateRole           = adminARNPrefix + "role/{name}"
	templatePolicies       = adminARNPrefix + "policy"
	templatePolicy         = adminARNPrefix + "policy/{name}"
	templateAudit          = adminARNPrefix + "audit"
	templateMetrics        = adminARNPrefix + "metrics"
	templateCluster        = adminARNPrefix + "cluster"
	templateSettings       = adminARNPrefix + "settings"
	templateTargets        = adminARNPrefix + "targets"
	templateTarget         = adminARNPrefix + "target/{name}"
	templateTagDefinitions = adminARNPrefix + "tag-definitions"
	templateTagDefinition  = adminARNPrefix + "tag-definition/{tag_key}"
	templateUpdates        = adminARNPrefix + "updates"
	templatePolicyCatalog  = adminARNPrefix + "policy-catalogue"
	templateAnonymous      = adminARNPrefix + "anonymous"
)

// ResourceKind describes one kind of resource a policy can name.
type ResourceKind struct {
	// Kind names the kind: "bucket", "user".
	Kind string
	// Template is the resource as a policy writes it, with placeholders in
	// braces for its names: "arn:aws:s3:::{bucket}". A policy may put
	// wildcards ('*', '?') in their place.
	Template string
	// Description says what the resource is.
	Description string
}

// ResourceKinds returns every kind of resource an action is asked on, and
// "*", which is every resource. Kinds are only ever added.
func ResourceKinds() []ResourceKind {
	return []ResourceKind{
		{"any", resourceAnyPattern, "every resource"},
		{"all-buckets", templateAllBuckets, "the deployment's buckets as a whole: what listing every bucket is asked on. Written in a policy, the same text is a pattern whose '*' matches every bucket and every object too"},
		{"bucket", templateBucket, "a bucket, by its name"},
		{"object", templateObject, "an object of a bucket, by its key; a listing is asked on the bucket"},
		{"users", templateUsers, "the users as a whole: what listing them is asked on"},
		{"user", templateUser, "a user, by name, with its API keys"},
		{"groups", templateGroups, "the groups as a whole: what listing them is asked on"},
		{"group", templateGroup, "a group, by name"},
		{"roles", templateRoles, "the roles as a whole: what listing them is asked on"},
		{"role", templateRole, "a role, by name"},
		{"policies", templatePolicies, "the named policies as a whole: what listing them is asked on"},
		{"policy", templatePolicy, "a named policy, by name"},
		{"audit", templateAudit, "the audit trail"},
		{"metrics", templateMetrics, "the metrics of a node"},
		{"cluster", templateCluster, "the nodes, drives and background work of the deployment"},
		{"settings", templateSettings, "how every node of the deployment is configured"},
		{"targets", templateTargets, "the targets as a whole: what listing them is asked on"},
		{"target", templateTarget, "a target the deployment sends to, by name"},
		{"tag-definitions", templateTagDefinitions, "the catalogue of tags as a whole: what listing it is asked on"},
		{"tag-definition", templateTagDefinition, "a definition of the catalogue of tags, by its tag key"},
		{"updates", templateUpdates, "the releases of the product and the upgrade of the deployment"},
		{"policy-catalogue", templatePolicyCatalog, "this catalogue"},
		{"anonymous", templateAnonymous, "a caller without credentials, as the subject of an explanation of access"},
	}
}

// entry is an action of the catalogue as it is written down: everything but
// the flags, which are the domain's own predicates.
type entry struct {
	name        Action
	group       string
	description string
	resources   []string
	read        bool
}

// Catalogue returns every action a policy can grant - each "s3:" and
// "admin:" action of this package, and nothing else - grouped, in a fixed
// order. The cluster actions are not in it (no policy grants them), nor are
// the names the audit trail gives to signing in or to the work of the
// system. A caller may change the slice it is given.
func Catalogue() []CatalogueAction {
	entries := append(s3Entries(), adminEntries()...)
	actions := make([]CatalogueAction, 0, len(entries))
	for _, e := range entries {
		_, inSubBucket := subBucketAction(string(e.name))
		actions = append(actions, CatalogueAction{
			Name: e.name, Group: e.group, Description: e.description, Resources: slices.Clone(e.resources), Read: e.read,
			ByNameOnly:        e.name.byNameOnly(),
			InBucketPolicy:    inBucketPolicy(e.name),
			Anonymous:         anonymousReadable(e.name),
			InSubBucketPolicy: inSubBucket,
		})
	}
	return actions
}

// LookupAction returns the catalogue's entry of an action named exactly as
// the catalogue names it. Any other name is no entry: one with a wildcard,
// one written in another case, a cluster action, a name of the audit trail.
func LookupAction(name Action) (CatalogueAction, bool) {
	for _, action := range Catalogue() {
		if action.Name == name {
			return action, true
		}
	}
	return CatalogueAction{}, false
}

// AskedOn reports whether the action is asked on the resource: the
// resource fits one of its templates (FitsTemplate).
func (a CatalogueAction) AskedOn(r Resource) bool {
	return slices.ContainsFunc(a.Resources, func(template string) bool { return FitsTemplate(template, r) })
}

// FitsTemplate reports whether a resource fits a template of the catalogue:
// with each placeholder replaced by one or more characters, the template is
// the resource's ARN (Resource.String). What a placeholder may hold:
//
//   - {bucket} and {name}: no '/' and no wildcard, '*' or '?'. A name stands
//     for one bucket or one entity, never for a pattern of them: the
//     resource of every bucket as a whole, "arn:aws:s3:::*", is no bucket,
//     and "user/*" is no user;
//   - {tag_key}: no wildcard, '/' allowed - a tag key may hold one
//     (object.ValidateTagKey), and "tag-definition/team/x" is the
//     definition of the key "team/x";
//   - {key}: anything. It is an object's key as it is, which S3 lets hold
//     any character, '/', '*' and '?' included: the object "a*" is asked
//     about as itself.
//
// Outside its placeholders a template is literal: "arn:aws:s3:::*" is
// fitted by the resource of every bucket as a whole and by nothing else.
func FitsTemplate(template string, r Resource) bool {
	return fits(template, r.String())
}

// fits matches an ARN against a template, placeholder by placeholder.
func fits(template, arn string) bool {
	open := strings.IndexByte(template, '{')
	if open < 0 {
		return template == arn
	}
	length := strings.IndexByte(template[open:], '}')
	if length < 0 || !strings.HasPrefix(arn, template[:open]) {
		return false
	}
	placeholder, rest, arn := template[open+1:open+length], template[open+length+1:], arn[open:]
	for n := 1; n <= len(arn); n++ {
		if !placeholderHolds(placeholder, arn[n-1]) {
			return false
		}
		if fits(rest, arn[n:]) {
			return true
		}
	}
	return false
}

// placeholderHolds reports whether a placeholder of a template may hold a
// byte (FitsTemplate).
func placeholderHolds(placeholder string, c byte) bool {
	switch placeholder {
	case "key":
		return true
	case "tag_key":
		return c != '*' && c != '?'
	default:
		return c != '/' && c != '*' && c != '?'
	}
}

// inBucketPolicy reports whether a bucket policy may allow the action: it is
// an S3 action, and not one only a user's own policy grants by name. This is
// the rule of ParseBucketPolicy; a test holds the two alike.
func inBucketPolicy(action Action) bool {
	return strings.HasPrefix(strings.ToLower(string(action)), "s3:") && !action.byNameOnly()
}

// s3Entries are the S3 actions of the catalogue.
func s3Entries() []entry {
	bucket, object := []string{templateBucket}, []string{templateObject}
	return []entry{
		{ActionListAllMyBuckets, groupBuckets, "List the buckets of the deployment.", []string{templateAllBuckets}, true},
		{ActionCreateBucket, groupBuckets, "Make a bucket.", bucket, false},
		{ActionDeleteBucket, groupBuckets, "Remove an empty bucket.", bucket, false},
		{ActionListBucket, groupBuckets, "List the objects of a bucket, or those under a prefix.", bucket, true},
		{ActionGetBucketLocation, groupBuckets, "Read the region a bucket is in.", bucket, true},

		{ActionGetObject, groupObjects, "Read the current version of an object, and copy it.", object, true},
		{ActionPutObject, groupObjects, "Store an object, by a single upload, a multipart upload or a copy.", object, false},
		{ActionDeleteObject, groupObjects, "Delete an object: remove it, or add a delete marker where versions are kept.", object, false},
		{ActionGetObjectTagging, groupObjects, "Read the tags of an object.", object, true},
		{ActionPutObjectTagging, groupObjects, "Set the tags of an object, as it is stored or afterwards.", object, false},
		{ActionDeleteObjectTagging, groupObjects, "Remove the tags of an object.", object, false},

		{ActionGetObjectVersion, groupVersions, "Read a version of an object by its ID.", object, true},
		{ActionDeleteObjectVersion, groupVersions, "Remove a version of an object for good.", object, false},
		{ActionListBucketVersions, groupVersions, "List the versions and delete markers of a bucket's objects.", bucket, true},
		{ActionGetBucketVersioning, groupVersions, "Read whether a bucket keeps versions.", bucket, true},
		{ActionPutBucketVersioning, groupVersions, "Turn the keeping of versions on or suspend it.", bucket, false},

		{ActionGetObjectRetention, groupObjectLock, "Read the retention of an object version.", object, true},
		{ActionPutObjectRetention, groupObjectLock, "Put a retention on an object version, or lengthen it.", object, false},
		{ActionGetObjectLegalHold, groupObjectLock, "Read whether an object version is under a legal hold.", object, true},
		{ActionPutObjectLegalHold, groupObjectLock, "Put a legal hold on an object version, or lift it.", object, false},
		{ActionGetBucketObjectLock, groupObjectLock, "Read the Object Lock configuration of a bucket.", bucket, true},
		{ActionPutBucketObjectLock, groupObjectLock, "Set the Object Lock configuration of a bucket and its default retention.", bucket, false},
		{ActionOverrideGovernance, groupObjectLock, "Shorten or lift a governance retention, and delete a version it keeps. Granted only by a statement that names it.", object, false},

		{ActionAbortMultipartUpload, groupMultipart, "Abort a multipart upload and discard its parts.", object, false},
		{ActionListMultipartUploadParts, groupMultipart, "List the parts of a multipart upload.", object, true},
		{ActionListBucketMultipartUploads, groupMultipart, "List the multipart uploads in progress in a bucket.", bucket, true},

		{ActionGetBucketPolicy, groupBucketConfig, "Read the policy of a bucket.", bucket, true},
		{ActionPutBucketPolicy, groupBucketConfig, "Set the policy of a bucket: whom it lets in, anyone included.", bucket, false},
		{ActionDeleteBucketPolicy, groupBucketConfig, "Remove the policy of a bucket.", bucket, false},
		{ActionGetBucketCORS, groupBucketConfig, "Read the CORS rules of a bucket.", bucket, true},
		{ActionPutBucketCORS, groupBucketConfig, "Set or remove the CORS rules of a bucket.", bucket, false},
		{ActionGetBucketTagging, groupBucketConfig, "Read the tags of a bucket.", bucket, true},
		{ActionPutBucketTagging, groupBucketConfig, "Set or remove the tags of a bucket.", bucket, false},
		{ActionGetBucketTagRules, groupBucketConfig, "Read the tag rules of a bucket.", bucket, true},
		{ActionPutBucketTagRules, groupBucketConfig, "Set or remove the tag rules of a bucket.", bucket, false},
		{ActionGetLifecycle, groupBucketConfig, "Read the lifecycle rules of a bucket.", bucket, true},
		{ActionPutLifecycle, groupBucketConfig, "Set or remove the lifecycle rules of a bucket.", bucket, false},
		{ActionGetBucketNotification, groupBucketConfig, "Read the notification rules of a bucket.", bucket, true},
		{ActionPutBucketNotification, groupBucketConfig, "Set or remove the notification rules of a bucket.", bucket, false},

		{ActionListSubBuckets, groupSubBuckets, "List every sub-bucket of a bucket; without it a caller sees those whose keys they may list.", bucket, true},
		{ActionGetSubBucketPolicy, groupSubBuckets, "Read a sub-bucket of a bucket and its policy.", bucket, true},
		{ActionPutSubBucket, groupSubBuckets, "Register a sub-bucket of a bucket and set its policy.", bucket, false},
		{ActionDeleteSubBucket, groupSubBuckets, "Remove an empty sub-bucket and its policy.", bucket, false},

		{ActionGetReplication, groupReplication, "Read the replication configuration of a bucket.", bucket, true},
		{ActionPutReplication, groupReplication, "Set or remove the replication configuration of a bucket.", bucket, false},
		{ActionReplicateObject, groupReplication, "Commit a version as the copy of another deployment's, with the ID and time it had there. Granted only by a statement that names it.", []string{templateBucket, templateObject}, false},
		{ActionReplicateDelete, groupReplication, "Commit a delete marker as the copy of another deployment's. Granted only by a statement that names it.", object, false},
	}
}

// adminEntries are the admin actions of the catalogue.
func adminEntries() []entry {
	bucket, object := []string{templateBucket}, []string{templateObject}
	user := []string{templateUser}
	return []entry{
		{ActionGetObjectAccess, groupObjects, "Read who uploaded an object version and who read it, how often and when.", []string{templateBucket, templateObject}, true},
		{ActionPreviewObject, groupObjects, "Have an object shown in the console; s3:GetObject is needed as well.", object, true},

		{ActionListUsers, groupUsers, "List the users.", []string{templateUsers}, true},
		{ActionGetUser, groupUsers, "Read a user: its groups, roles and whether it is disabled.", user, true},
		{ActionPutUser, groupUsers, "Make or change a user, its console password included.", user, false},
		{ActionDeleteUser, groupUsers, "Delete a user.", user, false},
		{ActionExplainAccess, groupUsers, "Ask what a user, an API key's owner, or a caller without credentials may do and which statements decide it.", []string{templateUser, templateUsers, templateAnonymous}, true},

		{ActionListGroups, groupGroups, "List the groups.", []string{templateGroups}, true},
		{ActionGetGroup, groupGroups, "Read a group.", []string{templateGroup}, true},
		{ActionPutGroup, groupGroups, "Make or change a group: its members and roles.", []string{templateGroup}, false},
		{ActionDeleteGroup, groupGroups, "Delete a group.", []string{templateGroup}, false},

		{ActionListRoles, groupRoles, "List the roles.", []string{templateRoles}, true},
		{ActionGetRole, groupRoles, "Read a role.", []string{templateRole}, true},
		{ActionPutRole, groupRoles, "Make or change a role: the policies it holds.", []string{templateRole}, false},
		{ActionDeleteRole, groupRoles, "Delete a role.", []string{templateRole}, false},

		{ActionListPolicies, groupPolicies, "List the named policies.", []string{templatePolicies}, true},
		{ActionGetPolicy, groupPolicies, "Read a named policy.", []string{templatePolicy}, true},
		{ActionPutPolicy, groupPolicies, "Make or change a named policy, or check one before it is saved.", []string{templatePolicy, templatePolicies}, false},
		{ActionDeletePolicy, groupPolicies, "Delete a named policy.", []string{templatePolicy}, false},
		{ActionGetPolicyCatalogue, groupPolicies, "Read this catalogue of actions, condition keys, resources and limits.", []string{templatePolicyCatalog}, true},

		{ActionListKeys, groupAPIKeys, "List the API keys of a user, without their secrets.", user, true},
		{ActionCreateKey, groupAPIKeys, "Make an API key for a user, with an inline policy or without.", user, false},
		{ActionDeleteKey, groupAPIKeys, "Revoke an API key of a user.", []string{templateUser, templateUsers}, false},

		{ActionGetAuditLog, groupAudit, "Read the audit trail, and define a target that is sent it.", []string{templateAudit}, true},

		{ActionGetClusterStatus, groupCluster, "Read the nodes, drives and background work of the deployment.", []string{templateCluster}, true},
		{ActionGetSettings, groupCluster, "Read how every node is configured: its settings, drives and certificates, without secrets.", []string{templateSettings}, true},
		{ActionGetMetrics, groupCluster, "Read the metrics of a node.", []string{templateMetrics}, true},

		{ActionGetBucketQuota, groupQuotas, "Read the quota of a bucket and its usage.", bucket, true},
		{ActionPutBucketQuota, groupQuotas, "Set the quota of a bucket.", bucket, false},
		{ActionDeleteBucketQuota, groupQuotas, "Remove the quota of a bucket.", bucket, false},
		{ActionRecountBucketUsage, groupQuotas, "Have the usage of a bucket counted again.", bucket, false},

		{ActionListTargets, groupTargets, "List the targets the deployment sends to.", []string{templateTargets}, true},
		{ActionGetTarget, groupTargets, "Read a target, without its credentials.", []string{templateTarget}, true},
		{ActionGetTargetStatus, groupTargets, "Read whether a target is being delivered to.", []string{templateTarget}, true},
		{ActionPutTarget, groupTargets, "Define or change a target: an address outside the deployment and the credential shown there.", []string{templateTarget}, false},
		{ActionDeleteTarget, groupTargets, "Delete a target.", []string{templateTarget}, false},

		{ActionGetReplicationStatus, groupReplication, "Read how far the replication of a bucket has come.", bucket, true},
		{ActionResyncReplication, groupReplication, "Start sending another deployment what it lacks of a bucket.", bucket, false},
		{ActionGetBucketReplicaMode, groupReplication, "Read whether a bucket is the replica of another deployment's.", bucket, true},
		{ActionPutBucketReplicaMode, groupReplication, "Make a bucket a replica, or promote it.", bucket, false},

		{ActionListTagDefinitions, groupTagCatalogue, "List the catalogue of tags.", []string{templateTagDefinitions}, true},
		{ActionGetTagDefinition, groupTagCatalogue, "Read a definition of the catalogue of tags.", []string{templateTagDefinition}, true},
		{ActionPutTagDefinition, groupTagCatalogue, "Add a definition to the catalogue of tags, or change one.", []string{templateTagDefinition}, false},
		{ActionDeleteTagDefinition, groupTagCatalogue, "Remove a definition no tag rule names.", []string{templateTagDefinition}, false},

		{ActionGetUpdates, groupUpdates, "Read the running build, the releases found and how far an upgrade has come.", []string{templateUpdates}, true},
		{ActionCheckForUpdates, groupUpdates, "Have the node ask the update source for releases now.", []string{templateUpdates}, false},
		{ActionUpgradeCluster, groupUpdates, "Install a release on every node; needs the password of a console session.", []string{templateUpdates}, false},
	}
}

// ValueKind is the kind of the values a condition key is compared with.
type ValueKind string

// The kinds of value.
const (
	// ValueIPPrefix is an IP address or a CIDR range, IPv4 written as IPv4,
	// without a zone.
	ValueIPPrefix ValueKind = "ip_prefix"
	// ValueBool is "true" or "false".
	ValueBool ValueKind = "bool"
	// ValueDateTime is a time in RFC 3339.
	ValueDateTime ValueKind = "date_time"
	// ValueString is a string; with StringLike and StringNotLike, '*' and
	// '?' are wildcards.
	ValueString ValueKind = "string"
)

// ConditionKey describes one condition key of the language.
type ConditionKey struct {
	// Key is the key as a policy names it: "aws:SourceIp".
	Key string
	// Operators are the operators that may test it, and no others.
	Operators []string
	// ValueKind is the kind of the values it is compared with.
	ValueKind ValueKind
	// WhenUnknown says what the condition does when the request does not
	// tell the key's value.
	WhenUnknown string
}

// ConditionKeys returns every condition key a policy may test, with the
// operators each may be tested with (operatorsFor). A key or an operator
// that is not here is refused by the parsers. A new key is a change of the
// language, not of the catalogue: an older node would refuse a stored policy
// that uses it (ADR-0032).
func ConditionKeys() []ConditionKey {
	return []ConditionKey{
		{
			Key: KeySourceIP, Operators: operatorsFor(KeySourceIP), ValueKind: ValueIPPrefix,
			WhenUnknown: "An unknown source address is decided against the request: the condition does not hold in an Allow, and holds in a Deny.",
		},
		{
			Key: KeySecureTransport, Operators: operatorsFor(KeySecureTransport), ValueKind: ValueBool,
			WhenUnknown: "Never unknown: a request arrived over TLS or it did not.",
		},
		{
			Key: KeyCurrentTime, Operators: operatorsFor(KeyCurrentTime), ValueKind: ValueDateTime,
			WhenUnknown: "Never unknown: it is the time of the deciding node's clock.",
		},
		{
			Key: KeyUsername, Operators: operatorsFor(KeyUsername), ValueKind: ValueString,
			WhenUnknown: `A caller who has not authenticated has no user name and is tested as the empty name "": StringEquals and StringLike match it only with "" or a pattern of stars, and StringNotEquals and StringNotLike hold otherwise.`,
		},
	}
}

// Limits are the bounds of a policy document, which bound the work of
// parsing it and of every decision made with it.
type Limits struct {
	// MaxBytes is the largest document, in bytes as sent.
	MaxBytes int
	// MaxStatements is the most statements a document may have.
	MaxStatements int
	// MaxValues is the most actions, resources, principals or values of
	// one condition a statement may have.
	MaxValues int
	// MaxDepth is how many levels below the document a value may be
	// nested.
	MaxDepth int
}

// PolicyLimits returns the bounds every parser of a policy enforces.
func PolicyLimits() Limits {
	return Limits{MaxBytes: MaxPolicyBytes, MaxStatements: MaxPolicyStatements, MaxValues: maxPolicyValues, MaxDepth: maxPolicyDepth}
}

// PrincipalForms are the forms the Principal of a statement of a bucket's or
// a sub-bucket's policy may take.
type PrincipalForms struct {
	// Anyone are the forms, as JSON text, that name every caller, anonymous
	// ones included.
	Anyone []string
	// UserARNPrefix begins the name of a user of this deployment: a
	// principal {"AWS": "<prefix><name>"} or a list of these and "*" under
	// "AWS".
	UserARNPrefix string
}

// BucketPolicyPrincipals returns the forms of a bucket policy's Principal
// (parsePrincipal): users and anyone, and no other kind.
func BucketPolicyPrincipals() PrincipalForms {
	return PrincipalForms{
		Anyone:        []string{`"*"`, `{"AWS":"*"}`, `{"AWS":["*"]}`},
		UserARNPrefix: userARNPrefix,
	}
}
