package adminapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// TestObjectReplicationStatus: the metadata of an object, as an upload
// answers it and as it is read back, says where the current version stands
// in replication (PLAN 8.1.10) - `PENDING` for a version a rule of the
// bucket matched, and nothing at all for a version no rule is about: the
// field is absent then, not empty.
func TestObjectReplicationStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, name := range []string{"photos", "plain"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
	}
	if err := f.meta.SetBucketVersioning(t.Context(), "photos", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/targets/site-b", body: siteTarget(t, "photos")})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/replication", body: map[string]any{
		"target": "site-b", "destination_bucket": "copy", "rules": []any{
			map[string]any{"id": "logs", "enabled": true, "priority": 0, "prefix": "logs/", "delete_markers": false},
		},
	}})

	for _, tc := range []struct {
		name   string
		bucket string
		key    string
		want   any // nil: the field is absent
	}{
		{name: "a version a rule matched", bucket: "photos", key: "logs/a.txt", want: "PENDING"},
		{name: "a version no rule matched", bucket: "photos", key: "other/a.txt"},
		{name: "a bucket without replication", bucket: "plain", key: "logs/a.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put := f.must(http.StatusOK, call{method: "PUT", path: "/buckets/" + tc.bucket + "/object?key=" + tc.key, raw: []byte("bytes")})
			read := f.must(http.StatusOK, call{method: "GET", path: "/buckets/" + tc.bucket + "/object/metadata?key=" + tc.key})
			for what, r := range map[string]reply{"the upload": put, "the metadata": read} {
				var body map[string]any
				if err := json.Unmarshal(r.body, &body); err != nil {
					t.Fatalf("%s: %v: %s", what, err, r.body)
				}
				got, present := body["replication_status"]
				switch {
				case tc.want == nil && present:
					t.Errorf("%s: replication_status = %v, want it absent", what, got)
				case tc.want != nil && got != tc.want:
					t.Errorf("%s: replication_status = %v, want %v", what, got, tc.want)
				}
			}
		})
	}
}
