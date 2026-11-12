package adminapi

// Conceal is conceal, for the tests of package adminapi_test.
func Conceal(pattern string, body []byte) ([]byte, []string) {
	return conceal(pattern, body)
}

// RequiredFields returns the fields the body of each operation must give.
func RequiredFields() map[string][]string {
	return requiredFields()
}

// AskedFirst returns the operations whose first question to the authorizer
// the request alone decides.
func AskedFirst() map[string]bool {
	out := map[string]bool{}
	for pattern := range askedFirstOf() {
		out[pattern] = true
	}
	return out
}

// ConcealedOperations returns the operations whose body is concealed whole.
func ConcealedOperations() map[string]bool {
	return concealedOperations()
}
