//go:build integration && rollingupgrade && !windows

package integration_test

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/test/clustertest"
)

// TestRollingUpgradeFromSchema3ToSchema4Settings: the settings of a
// deployment in the middle of a rolling upgrade (PLAN H.10), with the
// binaries themselves. The three voters start from the image
// $CACTUSFS_TEST_IMAGE_SCHEMA3 names - a release from before a node could be
// asked for its settings - and the fourth node from the image under test.
//
// The node of this release answers for all four: for itself, and of each node
// of the release before that it cannot be asked - "unsupported", which is
// neither unreachable nor failed, and nothing else of it. A node of the
// release before has no such endpoint. When one of them has been upgraded it
// answers like the node of this release, and the two that are left are still
// "unsupported", whichever of the two nodes of this release is asked.
//
// It bears the name of the rolling upgrade it is a part of, so that `make
// test-rolling`, which runs the tests of that name, runs it too. Without the
// image named it fails; it is never skipped.
func TestRollingUpgradeFromSchema3ToSchema4Settings(t *testing.T) {
	old := os.Getenv(schema3ImageEnv)
	if old == "" {
		t.Fatalf("%s names no image: this test asks nodes of the release of schema 3 for their settings, and needs an image of it", schema3ImageEnv)
	}
	t.Parallel()
	nodes, voters := clustertest.Nodes, clustertest.Nodes[:3]
	images := map[string]string{}
	for _, id := range voters {
		images[id] = old
	}
	c := clustertest.Start(t, clustertest.Options{Images: images})
	for _, id := range voters {
		if c.ImageOf(id) != old {
			t.Fatalf("%s runs %s, want %s", id, c.ImageOf(id), old)
		}
	}
	if c.ImageOf("node4") == old {
		t.Fatalf("the image under test is %s, the image of the release before: every node would be unsupported", old)
	}
	root := gridBearer(c.AccessKey, c.SecretKey)
	c.WaitReady(nodes...)

	// expect waits until a node of this release says which nodes answered
	// and which are of a release that cannot be asked, then asks once more
	// and holds that answer against the rest: the nodes of this release
	// with what they say of themselves, the others with the reason and
	// nothing else, and the deployment at the level of its oldest member.
	expect := func(asked string, upgraded ...string) {
		t.Helper()
		parts := make([]string, len(nodes))
		for i, id := range nodes {
			parts[i] = id + "=unsupported"
			if slices.Contains(upgraded, id) {
				parts[i] = id + "=answered"
			}
		}
		want, last := strings.Join(parts, " "), ""
		deadline := time.Now().Add(2 * time.Minute)
		for {
			view, _, err := settingsRead(t, c, asked, root)
			if err == nil && view.picture() == want {
				break
			}
			if err == nil {
				last = view.picture()
			}
			if time.Now().After(deadline) {
				t.Fatalf("the nodes through %s = %q (%v); waited two minutes for %q", asked, last, err, want)
			}
			time.Sleep(250 * time.Millisecond)
		}
		view, body, err := settingsRead(t, c, asked, root)
		if err != nil {
			t.Fatal(err)
		}
		if view.AnsweredBy != asked || view.picture() != want {
			t.Fatalf("asked again, the nodes through %s = %q, answered by %q; want %q", asked, view.picture(), view.AnsweredBy, want)
		}
		var members struct {
			Nodes []map[string]json.RawMessage `json:"nodes"`
		}
		if err := json.Unmarshal(body, &members); err != nil || len(members.Nodes) != len(nodes) {
			t.Fatalf("the nodes of the answer through %s: %v: %s", asked, err, body)
		}
		versions := map[string]bool{}
		for i, node := range view.Nodes {
			if !node.Answered {
				if len(members.Nodes[i]) != 3 {
					t.Errorf("%s, of the release before, is shown through %s with %d members; want its name, that it did not answer, and why: %s",
						node.NodeID, asked, len(members.Nodes[i]), body)
				}
				continue
			}
			if node.Server == nil || node.Server.Version == "" || len(node.Settings) == 0 || len(node.Drives) != 2 {
				t.Errorf("%s, of this release, through %s: build %+v, %d settings, %d drives", node.NodeID, asked, node.Server, len(node.Settings), len(node.Drives))
				continue
			}
			versions[node.Server.Version+"-"+node.Server.Build] = true
		}
		// While a member of the release before is left, the deployment is
		// at its level. Nothing is known of the settings of a node that did
		// not answer, and the nodes that did are of one build, configured
		// alike: there is no mismatch to report.
		_, levels, err := settingsCluster(t, c, asked, root)
		if err != nil {
			t.Fatal(err)
		}
		if d := view.Cluster; d.Nodes != 4 || d.Voters != 3 || d.Leader == "" || d.MetadataLevel != 3 || settingsLowest(levels) != 3 {
			t.Errorf("the deployment through %s = %+v, with the levels %v; want four nodes, three voters, a leader, level 3", asked, d, levels)
		}
		if len(versions) != 1 || len(view.Mismatches) != 0 {
			t.Errorf("through %s the nodes of this release are of %d builds and differ in %q; want one build and no mismatch", asked, len(versions), view.mismatches())
		}
	}

	// Three nodes of the release before, asked by the one of this release.
	expect("node4", "node4")
	// A node of the release before has no such endpoint, and says so as it
	// says it of any other it does not have.
	for _, id := range voters {
		status, header, body, err := settingsCall(t, c, id, root, http.MethodGet, "/settings", nil)
		var problem struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		}
		if err != nil || status != http.StatusNotFound || header.Get("Content-Type") != "application/problem+json" ||
			json.Unmarshal(body, &problem) != nil || problem.Status != http.StatusNotFound || problem.Code != "NotFound" {
			t.Errorf("GET /settings through %s, of the release before = %d %q %s (%v), want 404 and a problem", id, status, header.Get("Content-Type"), body, err)
		}
	}

	// One of them is upgraded: it answers, to itself and to the other node
	// of this release, and the two that are left are what they were.
	first := voters[0]
	c.Upgrade(first, "")
	c.WaitReady(first)
	if c.ImageOf(first) == old {
		t.Fatalf("%s still runs %s after its upgrade", first, old)
	}
	expect("node4", first, "node4")
	expect(first, first, "node4")
}
