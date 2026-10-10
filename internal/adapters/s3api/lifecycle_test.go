package s3api

import (
	"bytes"
	"net/http"
	"testing"
)

const lifecycleBody = `<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Rule><ID>logs</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status>
		<Expiration><Days>30</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays></NoncurrentVersionExpiration>
		<AbortIncompleteMultipartUpload><DaysAfterInitiation>2</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>
	<Rule><ID>old-style</ID><Prefix>tmp/</Prefix><Status>Disabled</Status><Expiration><Date>2027-01-01T00:00:00Z</Date></Expiration></Rule>
	<Rule><ID>markers</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>
</LifecycleConfiguration>`

func rule(inner string) []byte {
	return []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status>` + inner + `</Rule></LifecycleConfiguration>`)
}

// TestLifecycleOverHTTP: the lifecycle configuration of a bucket as S3
// clients set and read it, and what is refused.
func TestLifecycleOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?lifecycle"}), http.StatusNotFound, "NoSuchLifecycleConfiguration")
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/bkt?lifecycle", body: []byte(lifecycleBody)}), http.StatusOK)
	got := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?lifecycle"}), http.StatusOK)
	for _, want := range []string{
		"<ID>logs</ID>", "<Filter><Prefix>logs/</Prefix></Filter>", "<Status>Enabled</Status>", "<Expiration><Days>30</Days></Expiration>",
		"<NoncurrentDays>7</NoncurrentDays>", "<DaysAfterInitiation>2</DaysAfterInitiation>", "<Filter><Prefix>tmp/</Prefix></Filter>",
		"<Status>Disabled</Status>", "<Date>2027-01-01T00:00:00Z</Date>", "<ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>",
	} {
		if !bytes.Contains(got.body, []byte(want)) {
			t.Errorf("GetBucketLifecycleConfiguration lacks %s: %s", want, got.body)
		}
	}

	for name, tt := range map[string]struct {
		body   []byte
		status int
	}{
		// What is not implemented would, left out, make the rule remove
		// more than was meant: the whole configuration is refused.
		"a transition":            {rule(`<Transition><Days>30</Days><StorageClass>GLACIER</StorageClass></Transition>`), 501},
		"a noncurrent transition": {rule(`<NoncurrentVersionTransition><NoncurrentDays>1</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>`), 501},
		"a filter on a tag":       {rule(`<Filter><Tag><Key>a</Key><Value>b</Value></Tag></Filter><Expiration><Days>1</Days></Expiration>`), 501},
		"a filter of several":     {rule(`<Filter><And><Prefix>a</Prefix></And></Filter><Expiration><Days>1</Days></Expiration>`), 501},
		"a filter on size":        {rule(`<Filter><ObjectSizeGreaterThan>5</ObjectSizeGreaterThan></Filter><Expiration><Days>1</Days></Expiration>`), 501},
		"versions to keep":        {rule(`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>3</NewerNoncurrentVersions></NoncurrentVersionExpiration>`), 501},
		// A known element in a place it does not belong would be dropped
		// by a lenient reader, and the rule would then be about every key.
		"a prefix inside an expiration":  {rule(`<Expiration><Days>1</Days><Prefix>tmp/</Prefix></Expiration>`), 400},
		"a filter inside an expiration":  {rule(`<Expiration><Days>1</Days><Filter><Prefix>tmp/</Prefix></Filter></Expiration>`), 400},
		"a prefix among noncurrent days": {rule(`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><Prefix>tmp/</Prefix></NoncurrentVersionExpiration>`), 400},
		"a prefix among abort days":      {rule(`<AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation><Prefix>tmp/</Prefix></AbortIncompleteMultipartUpload>`), 400},
		"a prefix outside every rule":    {[]byte(`<LifecycleConfiguration><Prefix>tmp/</Prefix><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`), 400},
		"days inside a filter":           {rule(`<Filter><Prefix>tmp/</Prefix><Days>3</Days></Filter><Expiration><Days>1</Days></Expiration>`), 400},
		"a rule inside a rule":           {rule(`<Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule><Expiration><Days>1</Days></Expiration>`), 400},
		"an attribute on an element":     {[]byte(`<LifecycleConfiguration><Rule><Status x="Disabled">Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`), 400},
		"more days than a rule may name": {rule(`<Expiration><Days>200000</Days></Expiration>`), 400},
		"more noncurrent days than that": {rule(`<NoncurrentVersionExpiration><NoncurrentDays>106752</NoncurrentDays></NoncurrentVersionExpiration>`), 400},
		"more abort days than that":      {rule(`<AbortIncompleteMultipartUpload><DaysAfterInitiation>2147483647</DaysAfterInitiation></AbortIncompleteMultipartUpload>`), 400},
		"a transition, with a namespace": {rule(`<x:Transition xmlns:x="urn:x"><x:Days>1</x:Days></x:Transition>`), 501},
		"no rules":                       {[]byte(`<LifecycleConfiguration/>`), 400},
		"a rule that does nothing":       {rule(``), 400},
		"no status":                      {[]byte(`<LifecycleConfiguration><Rule><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`), 400},
		"a status that is none":          {[]byte(`<LifecycleConfiguration><Rule><Status>enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`), 400},
		"zero days":                      {rule(`<Expiration><Days>0</Days></Expiration>`), 400},
		"negative days":                  {rule(`<Expiration><Days>-3</Days></Expiration>`), 400},
		"days that are no number":        {rule(`<Expiration><Days>soon</Days></Expiration>`), 400},
		"days and a date":                {rule(`<Expiration><Days>1</Days><Date>2027-01-01T00:00:00Z</Date></Expiration>`), 400},
		"a date that is not midnight":    {rule(`<Expiration><Date>2027-01-01T10:00:00Z</Date></Expiration>`), 400},
		"a date that is none":            {rule(`<Expiration><Date>next year</Date></Expiration>`), 400},
		"zero noncurrent days":           {rule(`<NoncurrentVersionExpiration><NoncurrentDays>0</NoncurrentDays></NoncurrentVersionExpiration>`), 400},
		"zero days after initiation":     {rule(`<AbortIncompleteMultipartUpload><DaysAfterInitiation>0</DaysAfterInitiation></AbortIncompleteMultipartUpload>`), 400},
		"a prefix twice":                 {rule(`<Prefix>a</Prefix><Filter><Prefix>b</Prefix></Filter><Expiration><Days>1</Days></Expiration>`), 400},
		"two expirations":                {rule(`<Expiration><Days>1</Days></Expiration><Expiration><Days>9</Days></Expiration>`), 400},
		"an element not of S3":           {rule(`<Expiration><Days>1</Days></Expiration><Purge>true</Purge>`), 400},
		"not XML":                        {[]byte(`expire: 30d`), 400},
		"an ID twice":                    {[]byte(`<LifecycleConfiguration><Rule><ID>a</ID><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule><Rule><ID>a</ID><Status>Enabled</Status><Expiration><Days>2</Days></Expiration></Rule></LifecycleConfiguration>`), 400},
	} {
		if r := s.do(call{method: http.MethodPut, path: "/bkt?lifecycle", body: tt.body}); r.status != tt.status {
			t.Errorf("PutBucketLifecycleConfiguration with %s = %d %s, want %d", name, r.status, s.errorCode(r), tt.status)
		}
	}
	// None of them changed what was set.
	if after := s.mustStatus(s.do(call{method: http.MethodGet, path: "/bkt?lifecycle"}), http.StatusOK); !bytes.Equal(after.body, got.body) {
		t.Errorf("a refused configuration changed the rules: %s", after.body)
	}
	// Rules delete: a body the signature does not cover, or no signature,
	// sets none.
	s.expectError(s.do(call{method: http.MethodPut, path: "/bkt?lifecycle", body: rule(`<Expiration><Days>1</Days></Expiration>`), payloadHash: "UNSIGNED-PAYLOAD"}),
		http.StatusBadRequest, "InvalidRequest")
	for _, c := range []call{
		{method: http.MethodPut, path: "/bkt?lifecycle", body: rule(`<Expiration><Days>1</Days></Expiration>`), unsigned: true},
		{method: http.MethodGet, path: "/bkt?lifecycle", unsigned: true},
		{method: http.MethodDelete, path: "/bkt?lifecycle", unsigned: true},
	} {
		if r := s.do(c); r.status != http.StatusForbidden {
			t.Errorf("%s ?lifecycle without credentials = %d", c.method, r.status)
		}
	}
	s.expectError(s.do(call{method: http.MethodPut, path: "/missing?lifecycle", body: []byte(lifecycleBody)}), http.StatusNotFound, "NoSuchBucket")
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?lifecycle&acl"}), http.StatusNotImplemented, "NotImplemented")

	s.mustStatus(s.do(call{method: http.MethodDelete, path: "/bkt?lifecycle"}), http.StatusNoContent)
	s.expectError(s.do(call{method: http.MethodGet, path: "/bkt?lifecycle"}), http.StatusNotFound, "NoSuchLifecycleConfiguration")
}

// FuzzLifecycleConfiguration: what is read as lifecycle rules has no more
// rules than the document names, none of what is not implemented, and a
// place for every prefix the document has: a rule is never read as wider
// than it was written.
func FuzzLifecycleConfiguration(f *testing.F) {
	f.Add([]byte(lifecycleBody))
	f.Add(rule(`<Transition><Days>30</Days></Transition>`))
	f.Add(rule(`<Expiration><Days>1</Days><Prefix>tmp/</Prefix></Expiration>`))
	f.Add(rule(`<Expiration><Days>1</Days></Expiration><Filter><Prefix>&lt;Tag</Prefix></Filter>`))
	f.Add([]byte(`<!DOCTYPE a [<!ENTITY b "c">]><LifecycleConfiguration/>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var in lifecycleConfiguration
		if err := decodeShapedXML(bytes.NewReader(data), maxConfigBody, &in, lifecycleShape(), unsupportedLifecycle()...); err != nil {
			return
		}
		if bytes.Contains(data, []byte("<!DOCTYPE")) || len(in.Rules) > bytes.Count(data, []byte("<Rule")) {
			t.Fatalf("%d rules decoded from %q", len(in.Rules), data)
		}
		places := 0
		for _, r := range in.Rules {
			if r.Prefix != nil {
				places++
			}
			if r.Filter != nil {
				places++
			}
		}
		if written := bytes.Count(data, []byte("<Prefix")); written > places {
			t.Fatalf("the document has %d prefixes and the rules read have %d places for one: %q", written, places, data)
		}
	})
}
