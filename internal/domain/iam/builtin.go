package iam

// Built-in roles. Each has one built-in policy of the same name. They exist
// in every deployment, cannot be changed or removed, and are not stored:
// they are part of the binary, so that they mean the same everywhere and an
// upgrade can extend them.
const (
	RoleClusterAdmin = "cluster-admin"
	RoleBucketAdmin  = "bucket-admin"
	RoleReadWrite    = "read-write"
	RoleReadOnly     = "read-only"
	RoleAuditor      = "auditor"
)

func statement(actions, resources []string) Document {
	return Document{Version: PolicyVersion, Statements: []Statement{{Effect: Allow, Actions: actions, Resources: resources}}}
}

// BuiltInPolicies returns the built-in policies by name.
func BuiltInPolicies() map[string]Document {
	everything := []string{"*"}
	return map[string]Document{
		// Everything a user can be allowed: all of S3 and all of
		// administration. (Node RPC is not something a policy can grant.)
		RoleClusterAdmin: statement([]string{"s3:*", "admin:*"}, everything),
		// All of S3 on every bucket, and nothing of administration.
		RoleBucketAdmin: statement([]string{"s3:*"}, everything),
		// Read and write objects; buckets can be listed, not made or removed.
		RoleReadWrite: statement([]string{
			"s3:ListAllMyBuckets", "s3:ListBucket", "s3:GetBucketLocation",
			"s3:GetObject", "s3:PutObject", "s3:DeleteObject",
		}, everything),
		RoleReadOnly: statement([]string{
			"s3:ListAllMyBuckets", "s3:ListBucket", "s3:GetBucketLocation", "s3:GetObject",
		}, everything),
		// Look at how the deployment is run, change nothing, read no object.
		RoleAuditor: statement([]string{"admin:Get*", "admin:List*"}, everything),
	}
}

// BuiltInRoles returns the built-in roles by name.
func BuiltInRoles() map[string]Role {
	roles := map[string]Role{}
	for name := range BuiltInPolicies() {
		roles[name] = Role{Name: name, Policies: []string{name}, BuiltIn: true}
	}
	return roles
}

// IsBuiltIn reports whether name is the name of a built-in role and policy.
func IsBuiltIn(name string) bool {
	_, ok := BuiltInPolicies()[name]
	return ok
}
