package adminapi

// concealedOperations are the operations, as the router matches them, whose
// body may hold a secret: a field the contract marks `format: password` or
// `writeOnly` (a test holds the two together). Sign-in and unlocking carry a
// password too, and keep no fingerprint at all.
func concealedOperations() map[string]bool {
	return map[string]bool{
		"POST " + BasePath + "/users":         true,
		"PUT " + BasePath + "/users/{name}":   true,
		"PUT " + BasePath + "/targets/{name}": true,
	}
}

// conceal returns the body of a request as its fingerprint is made of, and
// the values taken out of it because they may be secrets.
//
// The body of an operation that may carry a secret is taken out WHOLE, as
// one value: "body=<the bytes as sent>". Picking the secret fields out of it
// would have to read the body as the generated decoder does - which matches
// field names without regard to case, folds "ſ" into "s", and merges keys
// that are there twice - and any way in which the two readings differ would
// leave a secret in what is hashed plainly. What is taken out is handed to
// the idempotency use case, which keeps none of it, nor a plain hash of it
// (app.IdempotentRequest.Concealed): it costs one Argon2id a request.
//
// The body of any other operation is returned as it is.
func conceal(pattern string, body []byte) ([]byte, []string) {
	if !concealedOperations()[pattern] {
		return body, nil
	}
	return nil, []string{"body=" + string(body)}
}
