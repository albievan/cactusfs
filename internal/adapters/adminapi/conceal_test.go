package adminapi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
)

const putTarget = "PUT " + adminapi.BasePath + "/targets/{name}"

// variant is a body of an operation that may carry a secret, written the
// ways the generated decoder reads alike - a name in another case, "ſ" for
// "s", a key twice - with the secret it carries.
type variant struct {
	method, path, body, secret string
}

// variants are the bodies that a choice of fields by their exact names
// would have left the secret in.
func variants() map[string]variant {
	target := func(name, body, secret string) variant {
		return variant{method: "PUT", path: "/targets/" + name, body: body, secret: secret}
	}
	user := func(body, secret string) variant {
		return variant{method: "POST", path: "/users", body: body, secret: secret}
	}
	const head = `{"kind":"webhook","endpoint":"https://hooks.example.com","buckets":[],`
	return map[string]variant{
		"Secret":             target("v-case", head+`"auth":{"mode":"bearer","Secret":"MARKED-variant-1"}}`, "MARKED-variant-1"),
		"AUTH":               target("v-upper", head+`"AUTH":{"mode":"bearer","secret":"MARKED-variant-2"}}`, "MARKED-variant-2"),
		"a long s":           target("v-long-s", head+`"auth":{"mode":"bearer","ſecret":"MARKED-variant-3"}}`, "MARKED-variant-3"),
		"Endpoint":           target("v-endpoint", `{"kind":"webhook","Endpoint":"https://hooks.example.com/MARKED-variant-4","buckets":[]}`, "MARKED-variant-4"),
		"auth twice":         target("v-twice", head+`"auth":{"mode":"bearer"},"auth":{"secret":"MARKED-variant-5"}}`, "MARKED-variant-5"),
		"Password":           user(`{"name":"bob","Password":"MARKED-variant-6"}`, "MARKED-variant-6"),
		"PASSWORD":           user(`{"name":"carl","PASSWORD":"MARKED-variant-7"}`, "MARKED-variant-7"),
		"a password twice":   user(`{"name":"dan","password":"x","password":"MARKED-variant-8"}`, "MARKED-variant-8"),
		"a body that is bad": target("v-bad", `{"auth":{"secret":"MARKED-variant-9"`, "MARKED-variant-9"),
	}
}

// TestConceal: the body of an operation that may carry a secret is taken
// out whole, however it is written; any other body is hashed as it is.
func TestConceal(t *testing.T) {
	t.Parallel()
	for name, v := range variants() {
		pattern := v.method + " " + adminapi.BasePath + "/targets/{name}"
		if v.path == "/users" {
			pattern = "POST " + adminapi.BasePath + "/users"
		}
		shown, concealed := adminapi.Conceal(pattern, []byte(v.body))
		if shown != nil || !slices.Equal(concealed, []string{"body=" + v.body}) {
			t.Errorf("%s: Conceal = %q %q, want the body taken out whole", name, shown, concealed)
		}
	}
	for _, pattern := range []string{putTarget, "POST " + adminapi.BasePath + "/users", "PUT " + adminapi.BasePath + "/users/{name}"} {
		if shown, concealed := adminapi.Conceal(pattern, []byte(`{"roles":[]}`)); shown != nil || len(concealed) != 1 {
			t.Errorf("%s: a body without a secret is not taken out whole: %q %q", pattern, shown, concealed)
		}
	}
	body := `{"name": "b",  "password":"not one"}`
	if shown, concealed := adminapi.Conceal("POST "+adminapi.BasePath+"/buckets", []byte(body)); string(shown) != body || concealed != nil {
		t.Errorf("an operation without secrets: Conceal = %q %q", shown, concealed)
	}
}

// contractSecrets returns, for each operation of an OpenAPI document in
// JSON, the fields of its request body that the document marks
// `format: password` or `writeOnly`, named by where they are:
// "auth.secret", "rules[].token", "labels.*.key". It follows `$ref` - to
// schemas and to request bodies - `properties`, `items`, `allOf`, `oneOf`,
// `anyOf` and `additionalProperties`, as deep as they go; a reference that
// leads back to one it came through is not followed again. Operations named
// in skip are left out.
func contractSecrets(t *testing.T, document []byte, skip ...string) map[string][]string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	// lookup follows a local JSON pointer, "#/components/schemas/Target".
	lookup := func(ref string) any {
		var at any = doc
		for _, step := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			node, _ := at.(map[string]any)
			at = node[strings.ReplaceAll(strings.ReplaceAll(step, "~1", "/"), "~0", "~")]
		}
		return at
	}
	join := func(at, name string) string {
		if at == "" {
			return name
		}
		return at + "." + name
	}
	var walk func(node any, at string, through map[string]bool, out *[]string)
	walk = func(node any, at string, through map[string]bool, out *[]string) {
		schema, ok := node.(map[string]any)
		if !ok {
			return
		}
		if ref, ok := schema["$ref"].(string); ok {
			if through[ref] {
				return
			}
			through[ref] = true
			walk(lookup(ref), at, through, out)
			delete(through, ref)
			return
		}
		if schema["format"] == "password" || schema["writeOnly"] == true {
			name := at
			if name == "" {
				name = "(the body)"
			}
			*out = append(*out, name)
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, property := range properties {
			walk(property, join(at, name), through, out)
		}
		walk(schema["items"], at+"[]", through, out)
		for _, combined := range []string{"allOf", "oneOf", "anyOf"} {
			list, _ := schema[combined].([]any)
			for _, part := range list {
				walk(part, at, through, out)
			}
		}
		walk(schema["additionalProperties"], join(at, "*"), through, out)
	}

	found := map[string][]string{}
	paths, _ := doc["paths"].(map[string]any)
	for path, raw := range paths {
		operations, _ := raw.(map[string]any)
		for method, raw := range operations {
			operation, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := operation["operationId"].(string); slices.Contains(skip, id) {
				continue
			}
			body, _ := operation["requestBody"].(map[string]any)
			if ref, ok := body["$ref"].(string); ok {
				body, _ = lookup(ref).(map[string]any)
			}
			content, _ := body["content"].(map[string]any)
			media, _ := content["application/json"].(map[string]any)
			var secrets []string
			walk(media["schema"], "", map[string]bool{}, &secrets)
			if len(secrets) > 0 {
				sort.Strings(secrets)
				found[strings.ToUpper(method)+" "+adminapi.BasePath+path] = secrets
			}
		}
	}
	return found
}

// TestTheWalkOfTheContractFindsEverySecret holds contractSecrets to account
// on a document made for it: a secret behind a request body that is a
// reference, an array, an allOf, a map, and a cycle.
func TestTheWalkOfTheContractFindsEverySecret(t *testing.T) {
	t.Parallel()
	document := []byte(`{
		"paths": {"/x": {"put": {"operationId": "putX", "requestBody": {"$ref": "#/components/requestBodies/X"}}},
		          "/y": {"post": {"operationId": "postY", "requestBody": {"content": {"application/json": {"schema": {
		              "oneOf": [{"type": "string"}, {"type": "object", "properties": {"pin": {"type": "string", "format": "password"}}}]}}}}}},
		          "/z": {"post": {"operationId": "postZ", "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Plain"}}}}}}},
		"components": {
			"requestBodies": {"X": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/A"}}}}},
			"schemas": {
				"A": {"type": "object", "properties": {"rules": {"type": "array", "items": {"$ref": "#/components/schemas/B"}}}},
				"B": {"allOf": [{"$ref": "#/components/schemas/C"}]},
				"C": {"type": "object", "properties": {
					"labels": {"type": "object", "additionalProperties": {"type": "object", "properties": {"key": {"type": "string", "writeOnly": true}}}},
					"back": {"$ref": "#/components/schemas/A"}}},
				"Plain": {"type": "object", "properties": {"name": {"type": "string"}, "self": {"$ref": "#/components/schemas/Plain"}}}
			}
		}
	}`)
	got := contractSecrets(t, document)
	want := map[string][]string{
		"PUT " + adminapi.BasePath + "/x":  {"rules[].labels.*.key"},
		"POST " + adminapi.BasePath + "/y": {"pin"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the walk found\n%v\nwant\n%v", got, want)
	}
}

// TestEverySecretOfTheContractIsConcealed: every operation whose request
// body has a field that the contract marks `format: password` or
// `writeOnly`, wherever in the body it is, has its body concealed - and no
// other operation.
func TestEverySecretOfTheContractIsConcealed(t *testing.T) {
	t.Parallel()
	// Sign-in and unlocking carry a password, and keep no answer: their
	// requests are carried out again whatever key they come with.
	want := contractSecrets(t, openapi.AdminJSON(), "signIn", "unlockSession")
	got := map[string]bool{}
	for pattern := range adminapi.ConcealedOperations() {
		got[pattern] = true
	}
	operations := map[string]bool{}
	for pattern := range want {
		operations[pattern] = true
	}
	if fmt.Sprint(got) != fmt.Sprint(operations) {
		t.Errorf("operations concealed\n%v\noperations whose bodies carry a secret (%v)\n%v", got, want, operations)
	}
	if len(want) == 0 {
		t.Fatal("the contract has no secret in a request body: the walk found nothing")
	}
}

// TestRepeatedRequestsKeepNoSecret: a repeated request with a secret is
// recognised - the same request is answered as before, the same key with
// another secret or another path is refused - and what is kept to recognise
// it holds neither the secret nor a plain hash of it or of the body.
func TestRepeatedRequestsKeepNoSecret(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	keyed := func(key string) map[string]string { return map[string]string{"Idempotency-Key": key} }
	const (
		hidden      = "MARKED-repeated-value-61c0"
		consoleWord = "MARKED-repeated-console-word-8e2a"
	)
	target := map[string]any{
		"kind": "webhook", "endpoint": "https://hooks.example.com/MARKED-path", "buckets": []string{},
		"auth": map[string]any{"mode": "bearer", "secret": hidden},
	}
	user := map[string]any{"name": "ann", "password": consoleWord}
	changed := map[string]any{"roles": []string{}, "groups": []string{}, "disabled": false, "password": consoleWord + "-changed"}
	for _, c := range []call{
		{method: "PUT", path: "/targets/hook", body: target, headers: keyed("target-1")},
		{method: "POST", path: "/users", body: user, headers: keyed("user-1")},
		{method: "PUT", path: "/users/ann", body: changed, headers: keyed("user-change-1")},
	} {
		first := f.must(f.do(withBearer(c, f.root)).status, c)
		again := f.must(first.status, c)
		if first.status >= 300 || again.header.Get("Idempotent-Replayed") != "true" || string(first.body) != string(again.body) {
			t.Errorf("%s %s and its repeat: %d %s / %d %s", c.method, c.path, first.status, first.body, again.status, again.body)
		}
	}
	for name, c := range map[string]call{
		"another secret": {method: "PUT", path: "/targets/hook", headers: keyed("target-1"), body: map[string]any{
			"kind": "webhook", "endpoint": "https://hooks.example.com/MARKED-path", "buckets": []string{},
			"auth": map[string]any{"mode": "bearer", "secret": hidden + "-not"},
		}},
		"another path": {method: "PUT", path: "/targets/hook", headers: keyed("target-1"), body: map[string]any{
			"kind": "webhook", "endpoint": "https://hooks.example.com/MARKED-other", "buckets": []string{},
			"auth": map[string]any{"mode": "bearer", "secret": hidden},
		}},
		"another password": {method: "POST", path: "/users", headers: keyed("user-1"), body: map[string]any{"name": "ann", "password": consoleWord + "-not"}},
		"another new password": {method: "PUT", path: "/users/ann", headers: keyed("user-change-1"), body: map[string]any{
			"roles": []string{}, "groups": []string{}, "disabled": false, "password": consoleWord + "-changed-not",
		}},
	} {
		if r := f.do(withBearer(c, f.root)); r.status != http.StatusUnprocessableEntity || r.problem(t)["code"] != "IdempotencyKeyReused" {
			t.Errorf("%s with the key of the first request = %d %s", name, r.status, r.body)
		}
	}

	targetBody, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	userBody, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	changedBody, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	plain := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	kept := f.watched.Kept()
	if len(kept) < 4 {
		t.Fatalf("%d records of repeated requests were kept, want at least 4", len(kept))
	}
	// Who reads a kept fingerprint and guesses the body cannot test the
	// guess with a hash: the fingerprint is not the one a plain hash of the
	// body, as the request was sent, gives - the hash of the use case
	// (length-prefixed SHA-256) over method, path, query and the body's
	// SHA-256.
	fingerprintOf := func(method, path string, body []byte) string {
		bodySum := sha256.Sum256(body)
		text := strings.Join([]string{method, adminapi.BasePath + path, "", hex.EncodeToString(bodySum[:])}, "\n")
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", len(text), text)))
		return hex.EncodeToString(sum[:])
	}
	guessable := map[string]bool{
		fingerprintOf("PUT", "/targets/hook", targetBody): true, fingerprintOf("POST", "/users", userBody): true,
		fingerprintOf("PUT", "/users/ann", changedBody): true,
	}
	// The rule above is the use case's: a body without a secret is
	// fingerprinted by it, as the repeat of an empty bucket list shows.
	plainBody := []byte(`{"name":"plain-bucket"}`)
	plainCall := call{method: "POST", path: "/buckets", body: map[string]any{"name": "plain-bucket"}, headers: keyed("bucket-1")}
	f.must(http.StatusCreated, plainCall)
	ruleHolds := false
	for _, record := range f.watched.Kept() {
		ruleHolds = ruleHolds || record.Fingerprint == fingerprintOf("POST", "/buckets", plainBody)
		if guessable[record.Fingerprint] {
			t.Errorf("a kept fingerprint is the plain hash of a body with a secret: %s", record.Fingerprint)
		}
	}
	if !ruleHolds {
		t.Fatal("the fingerprint of a body without a secret is not made as this test computes it: the check above proves nothing")
	}
	// The bodies written the ways the decoder reads alike: whatever the
	// operation answers them, what is kept of each is no plain hash of it.
	for name, v := range variants() {
		c := call{method: v.method, path: v.path, raw: []byte(v.body), headers: map[string]string{
			"Idempotency-Key": "variant-" + v.secret, "Content-Type": "application/json",
		}}
		if r := f.do(withBearer(c, f.root)); r.status >= 500 {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		guessable[fingerprintOf(v.method, v.path, []byte(v.body))] = true
	}
	kept = f.watched.Kept()
	for _, record := range kept {
		if guessable[record.Fingerprint] {
			t.Errorf("a kept fingerprint is the plain hash of a body with a secret: %s", record.Fingerprint)
		}
	}
	marks := map[string]string{}
	marks["the new password"] = consoleWord + "-changed"
	marks["the plain SHA-256 of the new password"] = plain(consoleWord + "-changed")
	marks["the plain SHA-256 of the body of the change of the user"] = plain(string(changedBody))
	for name, v := range variants() {
		marks["the secret of "+name] = v.secret
		marks["the plain SHA-256 of the secret of "+name] = plain(v.secret)
		marks["the plain SHA-256 of the body of "+name] = plain(v.body)
	}
	dump := fmt.Sprintf("%+v", kept)
	for name, mark := range marks {
		if strings.Contains(dump, mark) {
			t.Errorf("what is kept of the repeated requests holds %s", name)
		}
	}
	for name, mark := range map[string]string{
		"the secret": hidden, "the password": consoleWord, "the path": "MARKED",
		"the plain SHA-256 of the secret": plain(hidden), "the plain SHA-256 of the password": plain(consoleWord),
		"the plain SHA-256 of the body of the target": plain(string(targetBody)), "the plain SHA-256 of the body of the user": plain(string(userBody)),
	} {
		if strings.Contains(dump, mark) {
			t.Errorf("what is kept of the repeated requests holds %s", name)
		}
	}
}
