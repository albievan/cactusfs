package adminapi

// Conceal is conceal, for the tests of package adminapi_test.
func Conceal(pattern string, body []byte) ([]byte, []string) {
	return conceal(pattern, body)
}

// ConcealedOperations returns the operations whose body is concealed whole.
func ConcealedOperations() map[string]bool {
	return concealedOperations()
}
