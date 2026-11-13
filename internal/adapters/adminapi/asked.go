package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// askedFirst is what an operation asks the authorizer before anything else:
// an action on the administrative resource kind/<name>, with the name from
// the path - or from the body, for an operation that makes the resource -
// or on the resource kind itself, for one that has no name.
type askedFirst struct {
	action iam.Action
	kind   string
	// fromBody says that the name is the body's "name".
	fromBody bool
	// whole says that the resource is kind itself, with no name.
	whole bool
}

// askedFirstOf lists, by the operation as the router matches it, the
// operations whose first question to the authorizer the request alone
// decides: those on users, groups, roles, policies and targets by name, and
// asking for a check for releases now. Each
// row is what the use case behind the operation asks, as it asks it (a test
// holds them together). It is given to the idempotency use case, which asks
// it before a key is reserved or a body concealed (app.IdempotentRequest):
// the operation still asks, and decides. An operation whose question
// depends on more than the request - the owner of a key, the policy of a
// bucket - is not listed, and is reserved before it is asked: deleting an
// API key (its owner) and every operation on a bucket (its policy, which the
// authorizer reads). Starting an upgrade takes no idempotency key at all.
func askedFirstOf() map[string]askedFirst {
	return map[string]askedFirst{
		"POST " + BasePath + "/users":             {iam.ActionPutUser, "user", true, false},
		"PUT " + BasePath + "/users/{name}":       {iam.ActionPutUser, "user", false, false},
		"DELETE " + BasePath + "/users/{name}":    {iam.ActionDeleteUser, "user", false, false},
		"POST " + BasePath + "/users/{name}/keys": {iam.ActionCreateKey, "user", false, false},
		"POST " + BasePath + "/groups":            {iam.ActionPutGroup, "group", true, false},
		"PUT " + BasePath + "/groups/{name}":      {iam.ActionPutGroup, "group", false, false},
		"DELETE " + BasePath + "/groups/{name}":   {iam.ActionDeleteGroup, "group", false, false},
		"POST " + BasePath + "/roles":             {iam.ActionPutRole, "role", true, false},
		"PUT " + BasePath + "/roles/{name}":       {iam.ActionPutRole, "role", false, false},
		"DELETE " + BasePath + "/roles/{name}":    {iam.ActionDeleteRole, "role", false, false},
		"POST " + BasePath + "/policies":          {iam.ActionPutPolicy, "policy", true, false},
		"PUT " + BasePath + "/policies/{name}":    {iam.ActionPutPolicy, "policy", false, false},
		"DELETE " + BasePath + "/policies/{name}": {iam.ActionDeletePolicy, "policy", false, false},
		"PUT " + BasePath + "/targets/{name}":     {iam.ActionPutTarget, "target", false, false},
		"DELETE " + BasePath + "/targets/{name}":  {iam.ActionDeleteTarget, "target", false, false},
		"POST " + BasePath + "/updates/check":     {iam.ActionCheckForUpdates, "updates", false, true},
	}
}

// firstQuestion returns the action and the resource the operation of r asks
// the authorizer first, or an empty action where the request does not say.
// body is the request's body, as read.
func firstQuestion(r *http.Request, body []byte) (iam.Action, iam.Resource) {
	asked, ok := askedFirstOf()[r.Pattern]
	if !ok {
		return "", iam.Resource{}
	}
	if asked.whole {
		return asked.action, iam.Resource{Admin: asked.kind}
	}
	name := r.PathValue("name")
	if asked.fromBody {
		// Read as the generated decoder reads it - encoding/json into a
		// field tagged "name" - so that the name is the one the operation
		// is given. A body that does not read is the decoder's to refuse.
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &named) != nil {
			return "", iam.Resource{}
		}
		name = named.Name
	}
	if name == "" {
		return "", iam.Resource{}
	}
	return asked.action, iam.Resource{Admin: asked.kind + "/" + name}
}
