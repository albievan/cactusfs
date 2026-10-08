package object_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestSSEValidate(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{7}, object.KeySize)
	many := map[string]string{}
	for i := range 17 {
		many[strings.Repeat("k", i+1)] = "v"
	}
	tests := []struct {
		name string
		sse  object.SSE
		ok   bool
	}{
		{"the default", object.SSE{}, true},
		{"SSE-KMS", object.SSE{Mode: object.SSEKMS}, true},
		{"SSE-KMS with a key and a context", object.SSE{Mode: object.SSEKMS, KMSKeyID: "k", Context: map[string]string{"a": "b"}}, true},
		{"SSE-C", object.SSE{Mode: object.SSEC, CustomerKey: key}, true},
		{"another mode", object.SSE{Mode: "aws:kms:dsse"}, false},
		{"a context without SSE-KMS", object.SSE{Context: map[string]string{"a": "b"}}, false},
		{"a KMS key without SSE-KMS", object.SSE{KMSKeyID: "k"}, false},
		{"a customer key without SSE-C", object.SSE{CustomerKey: key}, false},
		{"a customer key with SSE-KMS", object.SSE{Mode: object.SSEKMS, CustomerKey: key}, false},
		{"SSE-C without a key", object.SSE{Mode: object.SSEC}, false},
		{"SSE-C with a short key", object.SSE{Mode: object.SSEC, CustomerKey: key[:16]}, false},
		{"SSE-C with a context", object.SSE{Mode: object.SSEC, CustomerKey: key, Context: map[string]string{"a": "b"}}, false},
		{"too many pairs", object.SSE{Mode: object.SSEKMS, Context: many}, false},
		{"too large a context", object.SSE{Mode: object.SSEKMS, Context: map[string]string{"a": strings.Repeat("x", 3000)}}, false},
	}
	for _, tt := range tests {
		err := tt.sse.Validate()
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("%s: %v", tt.name, err)
		}
		var enc object.Encryption
		if applyErr := tt.sse.Apply(&enc); (applyErr == nil) != tt.ok {
			t.Errorf("%s: Apply = %v", tt.name, applyErr)
		}
	}
}

// TestSSEApplyRecordsNoKey: what is recorded of a request is its mode, its
// context in one canonical form, and the digest of a customer key.
func TestSSEApplyRecordsNoKey(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{7}, object.KeySize)
	var enc object.Encryption
	if err := (object.SSE{Mode: object.SSEC, CustomerKey: key}).Apply(&enc); err != nil {
		t.Fatal(err)
	}
	if enc.Mode != object.SSEC || len(enc.Context) != 0 || len(enc.WrappedKey) != 0 || len(enc.KeyCheck) != 0 {
		t.Errorf("recorded = %+v", enc)
	}
	a, b := object.Encryption{}, object.Encryption{}
	if err := (object.SSE{Mode: object.SSEKMS, Context: map[string]string{"b": "2", "a": "1"}}).Apply(&a); err != nil {
		t.Fatal(err)
	}
	if err := (object.SSE{Mode: object.SSEKMS, Context: map[string]string{"a": "1", "b": "2"}}).Apply(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Context, b.Context) || string(a.Context) != `{"a":"1","b":"2"}` {
		t.Errorf("contexts %s and %s", a.Context, b.Context)
	}
	if got := a.ContextMap(); len(got) != 2 || got["b"] != "2" {
		t.Errorf("ContextMap = %v", got)
	}
	if (object.Encryption{}).ContextMap() != nil || (object.Encryption{Context: []byte("{")}).ContextMap() != nil {
		t.Error("ContextMap of nothing")
	}
}

// TestKeyContext: the context a key is wrapped under changes with the mode
// and the encryption context, and not for the default, whose keys were
// wrapped before there were modes.
func TestKeyContext(t *testing.T) {
	t.Parallel()
	base := []byte("0123456789abcdef")
	if got := (object.Encryption{}).KeyContext(base); !bytes.Equal(got, base) {
		t.Errorf("default context = %q", got)
	}
	contexts := map[string][]byte{}
	for name, enc := range map[string]object.Encryption{
		"default":         {},
		"kms":             {Mode: object.SSEKMS},
		"kms, a context":  {Mode: object.SSEKMS, Context: []byte(`{"a":"1"}`)},
		"kms, another":    {Mode: object.SSEKMS, Context: []byte(`{"a":"2"}`)},
		"customer":        {Mode: object.SSEC},
		"customer, check": {Mode: object.SSEC, KeyCheck: []byte{1}},
	} {
		contexts[name] = enc.KeyContext(base)
	}
	for a, ca := range contexts {
		for b, cb := range contexts {
			same := a == b || (strings.HasPrefix(a, "customer") && strings.HasPrefix(b, "customer"))
			if bytes.Equal(ca, cb) != same {
				t.Errorf("contexts of %q and %q equal = %v", a, b, !same)
			}
		}
	}
	// It does not write into the base it was given.
	withRoom := append(make([]byte, 0, 200), base...)
	_ = (object.Encryption{Mode: object.SSEKMS}).KeyContext(withRoom)
	if !bytes.Equal(withRoom[:cap(withRoom)][len(base):len(base)+4], make([]byte, 4)) {
		t.Error("KeyContext wrote past the base")
	}
}

// TestCustomerDataKey: the key an SSE-C object is encrypted with takes both
// keys, is neither, and is checked without revealing either.
func TestCustomerDataKey(t *testing.T) {
	t.Parallel()
	property := func(kmsA, kmsB, customerA, customerB [object.KeySize]byte) bool {
		a, errA := object.CustomerDataKey(kmsA[:], customerA[:])
		again, _ := object.CustomerDataKey(kmsA[:], customerA[:])
		otherKMS, _ := object.CustomerDataKey(kmsB[:], customerA[:])
		otherCustomer, _ := object.CustomerDataKey(kmsA[:], customerB[:])
		swapped, _ := object.CustomerDataKey(customerA[:], kmsA[:])
		if errA != nil || len(a) != object.KeySize || !bytes.Equal(a, again) || bytes.Equal(a, kmsA[:]) || bytes.Equal(a, customerA[:]) {
			return false
		}
		if (kmsA == kmsB) != bytes.Equal(a, otherKMS) || (customerA == customerB) != bytes.Equal(a, otherCustomer) {
			return false
		}
		if kmsA != customerA && bytes.Equal(a, swapped) {
			return false
		}
		check := object.KeyCheck(a)
		if len(check) != 16 || bytes.Contains(a, check) || object.VerifyKeyCheck(a, check) != nil {
			return false
		}
		return customerA == customerB || errors.Is(object.VerifyKeyCheck(otherCustomer, check), domain.ErrCustomerKeyMismatch)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
	key := make([]byte, object.KeySize)
	if _, err := object.CustomerDataKey(key[:8], key); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a short KMS key = %v", err)
	}
	if _, err := object.CustomerDataKey(key, key[:31]); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a short customer key = %v", err)
	}
	derived, _ := object.CustomerDataKey(key, key)
	for _, bad := range [][]byte{nil, {}, object.KeyCheck(derived)[:8]} {
		if err := object.VerifyKeyCheck(derived, bad); !errors.Is(err, domain.ErrCustomerKeyMismatch) {
			t.Errorf("check %x accepted", bad)
		}
	}
}

// FuzzKeyContext: two records that differ in the ID, the mode or the context
// are never wrapped under the same binding, and the ID given is not written
// to.
func FuzzKeyContext(f *testing.F) {
	f.Add([]byte("0123456789abcdef"), "aws:kms", []byte(`{"a":"b"}`), []byte("0123456789abcdef"), "aws:kms", []byte(`{"a":"c"}`))
	f.Add([]byte("ab"), "sse-c", []byte(""), []byte("a"), "sse-c", []byte("b"))
	f.Add([]byte("a"), "aws:kms\x00", []byte("x"), []byte("a"), "aws:kms", []byte("\x00x"))
	f.Fuzz(func(t *testing.T, baseA []byte, modeA string, contextA []byte, baseB []byte, modeB string, contextB []byte) {
		a := object.Encryption{Mode: modeA, Context: contextA}
		b := object.Encryption{Mode: modeB, Context: contextB}
		if modeA == object.SSES3 || modeB == object.SSES3 {
			// The default has no mode and no context to bind.
			return
		}
		before := append([]byte{}, baseA...)
		ca, cb := a.KeyContext(baseA), b.KeyContext(baseB)
		if !bytes.Equal(baseA, before) {
			t.Fatal("KeyContext wrote to the base it was given")
		}
		same := bytes.Equal(baseA, baseB) && modeA == modeB && bytes.Equal(contextA, contextB)
		if bytes.Equal(ca, cb) != same {
			t.Fatalf("contexts equal = %v for (%q, %q, %q) and (%q, %q, %q)", !same, baseA, modeA, contextA, baseB, modeB, contextB)
		}
	})
}

// TestOpaqueTag: the entity tag of an object under SSE-C is as long as the
// one it replaces, the same for the same content and key, and no digest of
// the content.
func TestOpaqueTag(t *testing.T) {
	t.Parallel()
	const md5OfHello = "5d41402abc4b2a76b9719d911017c592"
	keyA, keyB := bytes.Repeat([]byte{1}, object.KeySize), bytes.Repeat([]byte{2}, object.KeySize)
	a, err := object.OpaqueTag(keyA, md5OfHello)
	again, _ := object.OpaqueTag(keyA, md5OfHello)
	otherKey, _ := object.OpaqueTag(keyB, md5OfHello)
	otherContent, _ := object.OpaqueTag(keyA, "7d793037a0760186574b0282f2f435e7")
	if err != nil || len(a) != len(md5OfHello) || a != again || a == md5OfHello || a == otherKey || a == otherContent {
		t.Errorf("tags %q %q %q %q, %v", a, again, otherKey, otherContent, err)
	}
	if !object.KnownMode(object.SSES3) || !object.KnownMode(object.SSEKMS) || !object.KnownMode(object.SSEC) || object.KnownMode("aws:kms:dsse") {
		t.Error("KnownMode")
	}
	if len(object.KeyCheck(keyA)) != 16 || bytes.Equal(object.KeyCheck(keyA), object.KeyCheck(keyB)) {
		t.Error("KeyCheck")
	}
}
