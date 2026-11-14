//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/albievan/cactusfs/test/clustertest"
)

const (
	// tagBucket is the bucket of TestTagRulesAcrossTheCluster that has tag
	// rules.
	tagBucket = "tagged"
	// plainBucket is one made with S3's CreateBucket, without rules: its
	// object with a tag outside the catalogue is a source of copies.
	plainBucket = "plain"
	// plainSource is that object, tagged `Colour=Red`.
	plainSource = "coloured"
)

// mustRefuseTag judges the answer to a request whose tags break the bucket's
// rules: nil for 400 InvalidTag; a plain error when the server gave no answer
// or one of 5xx, which an election may cause; errWrongAnswer for anything
// else - a 2xx stores what the rules forbid, and another 4xx (403, 404) is
// not the refusal either.
func mustRefuseTag(what string, err error) error {
	status, code := sdkStatus(err), apiCode(err)
	switch {
	case status == http.StatusBadRequest && code == "InvalidTag":
		return nil
	case unanswered(status):
		return errors.Join(fmt.Errorf("%s: not answered yet (status %d)", what, status), err)
	default:
		return errors.Join(fmt.Errorf("%w: %s = %d %q, want 400 InvalidTag", errWrongAnswer, what, status, code), err)
	}
}

// unanswered says whether a status is no answer, or one of 5xx, which an
// election may cause.
func unanswered(status int) bool {
	return status == 0 || status >= http.StatusInternalServerError
}

// tagsAre reads the tags of an object and compares them with want: nil when
// they are want, a plain error when they could not be read, errWrongAnswer
// when they are other tags.
func tagsAre(ctx context.Context, client *s3.Client, what, key string, want map[string]string) error {
	out, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(tagBucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("GetObjectTagging %s/%s (%s): %w", tagBucket, key, what, err)
	}
	if got := tagsOf(out.TagSet); len(out.TagSet) != len(want) || !maps.Equal(got, want) {
		return fmt.Errorf("%w: GetObjectTagging %s/%s (%s) = %v, want %v", errWrongAnswer, tagBucket, key, what, got, want)
	}
	return nil
}

// notStored says that a refused write left nothing behind: nil for 404
// NoSuchKey - not NoSuchBucket, which would say nothing of the key - a plain
// error when the server gave no answer or one of 5xx, errWrongAnswer for
// the object or any other answer.
func notStored(ctx context.Context, client *s3.Client, what, key string) error {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(tagBucket), Key: aws.String(key)})
	if err == nil {
		_ = out.Body.Close()
		return fmt.Errorf("%w: GetObject %s/%s after %s answered the object: the refused write was stored", errWrongAnswer, tagBucket, key, what)
	}
	switch status, code := sdkStatus(err), apiCode(err); {
	case status == http.StatusNotFound && code == "NoSuchKey":
		return nil
	case unanswered(status):
		return errors.Join(fmt.Errorf("GetObject %s/%s after %s: not answered yet (status %d)", tagBucket, key, what, status), err)
	default:
		return errors.Join(fmt.Errorf("%w: GetObject %s/%s after %s = %d %q, want 404 NoSuchKey", errWrongAnswer, tagBucket, key, what, status, code), err)
	}
}

// tagUpload sends a multipart upload of one part with the tags given at its
// creation, and completes it. Whatever fails is said as a plain error: an
// upload is a request that is allowed.
func tagUpload(ctx context.Context, client *s3.Client, key, tagging string, data []byte) (err error) {
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(tagBucket), Key: aws.String(key), Tagging: aws.String(tagging),
	})
	if err != nil {
		return fmt.Errorf("CreateMultipartUpload %s/%s with %s: %w", tagBucket, key, tagging, err)
	}
	// An upload that does not complete is taken back, so that a round
	// that is repeated leaves none behind; that it could not be is no
	// answer of the test.
	defer func() {
		if err != nil {
			_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket: aws.String(tagBucket), Key: aws.String(key), UploadId: created.UploadId,
			})
		}
	}()
	sent, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(tagBucket), Key: aws.String(key), UploadId: created.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("UploadPart of %s/%s: %w", tagBucket, key, err)
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(tagBucket), Key: aws.String(key), UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: sent.ETag, ChecksumCRC32: sent.ChecksumCRC32},
		}},
	}); err != nil {
		return fmt.Errorf("CompleteMultipartUpload of %s/%s: %w", tagBucket, key, err)
	}
	return nil
}

// adminUpload stores an object of the test's bucket through the management
// API of a node (putObject), with the tags of X-CactusFS-Tagging when tagging
// is not empty, and returns the status, the headers of the answer and its
// body.
func adminUpload(ctx context.Context, c *clustertest.Cluster, id string, headers map[string]string, key, tagging string, data []byte) (int, http.Header, []byte, error) {
	target := "https://" + id + ":9001/api/v1/buckets/" + tagBucket + "/object?key=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("upload %s/%s: %w", tagBucket, key, err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if tagging != "" {
		req.Header.Set("X-CactusFS-Tagging", tagging)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.Admin(id).Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("upload %s/%s through %s: %w", tagBucket, key, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("upload %s/%s through %s: %w", tagBucket, key, id, err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// tagRound is what one node's rounds of tagRuleAnswers carry from one round
// to the next.
type tagRound struct {
	// unsure is set when a change of the draft's tags that the rules
	// forbid got no answer: it may have been committed all the same, so
	// the tags are read before the draft is put again and overwrites them.
	unsure bool
}

// tagRuleAnswers is what S3 and the management API of one node answer about
// the tags of the test's bucket, whose rules lower-case tags, take only tags
// of the catalogue and require `stage` to be `draft` or `final`; nil when
// every answer is the one expected. The keys it writes begin with the node's
// name, so that a round through another node writes keys of its own. Every
// request is made whatever came before - but for those about the round's
// draft when its put got no answer: they would be answered NoSuchKey - so
// that a wrong answer is seen even in a round where another request was not
// answered; the error wraps errWrongAnswer when any answer was wrong.
func tagRuleAnswers(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string, client *s3.Client, round *tagRound, content []byte) error {
	var errs []error
	draft := id + "/draft"

	// A forbidden change of the draft's tags that got no answer in the
	// round before is looked for before the draft is put again.
	if round.unsure {
		if err := tagsAre(ctx, client, "after a change of its tags that got no answer, through "+id, draft, map[string]string{"stage": "draft"}); err != nil {
			return err
		}
		round.unsure = false
	}

	// Stored, and kept in lower case.
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(tagBucket), Key: aws.String(draft), Body: bytes.NewReader(content), Tagging: aws.String("Stage=Draft"),
	})
	stored := err == nil
	if err != nil {
		errs = append(errs, fmt.Errorf("PutObject %s/%s with Stage=Draft through %s: %w", tagBucket, draft, id, err))
	} else {
		errs = append(errs, tagsAre(ctx, client, "put with Stage=Draft through "+id, draft, map[string]string{"stage": "draft"}))
		switch got, err := gridGet(ctx, client, tagBucket, draft); {
		case err != nil:
			errs = append(errs, fmt.Errorf("through %s: %w", id, err))
		case !bytes.Equal(got, content):
			errs = append(errs, fmt.Errorf("%w: GetObject %s/%s through %s: the content differs", errWrongAnswer, tagBucket, draft, id))
		}
	}

	// Refused, and nothing stored: no tags where one is required, a tag
	// the catalogue does not define, a value the rule does not allow, and
	// two keys that are one key in lower case.
	for _, put := range []struct {
		name, tagging string
	}{
		{"untagged", ""},
		{"outside", "Stage=Draft&Colour=Red"},
		{"published", "Stage=Published"},
		{"twice", "Stage=Draft&stage=final"},
	} {
		key := id + "/" + put.name
		in := &s3.PutObjectInput{Bucket: aws.String(tagBucket), Key: aws.String(key), Body: bytes.NewReader(content)}
		if put.tagging != "" {
			in.Tagging = aws.String(put.tagging)
		}
		_, err := client.PutObject(ctx, in)
		what := fmt.Sprintf("PutObject %s/%s with tags %q through %s", tagBucket, key, put.tagging, id)
		if unexpected := mustRefuseTag(what, err); unexpected != nil {
			errs = append(errs, unexpected)
			continue
		}
		errs = append(errs, notStored(ctx, client, what, key))
	}

	// The management API's upload is held to the same rules: refused with
	// its own problem code without the required tag, stored in lower case
	// with it.
	unstaged := id + "/admin-untagged"
	switch status, header, body, err := adminUpload(ctx, c, id, root, unstaged, "", content); {
	case status == http.StatusBadRequest && previewProblem(header, body) == "TagRuleViolated":
		errs = append(errs, notStored(ctx, client, "an upload through the management API without tags", unstaged))
	case unanswered(status):
		errs = append(errs, errors.Join(fmt.Errorf("upload %s/%s without tags through %s: not answered yet (status %d)", tagBucket, unstaged, id, status), err))
	default:
		errs = append(errs, errors.Join(fmt.Errorf("%w: upload %s/%s without tags through %s = %d %s, want 400 TagRuleViolated", errWrongAnswer, tagBucket, unstaged, id, status, body), err))
	}
	staged := id + "/admin-final"
	switch status, _, body, err := adminUpload(ctx, c, id, root, staged, "Stage=Final", content); {
	case err != nil:
		errs = append(errs, err)
	case status != http.StatusOK:
		errs = append(errs, fmt.Errorf("upload %s/%s with Stage=Final through %s = %d %s", tagBucket, staged, id, status, body))
	default:
		errs = append(errs, tagsAre(ctx, client, "upload through the management API with Stage=Final through "+id, staged, map[string]string{"stage": "final"}))
	}

	// A multipart upload is held to the rules at its creation, and keeps
	// the tags it was begun with.
	untagged := id + "/upload-untagged"
	_, err = client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(tagBucket), Key: aws.String(untagged)})
	errs = append(errs, mustRefuseTag(fmt.Sprintf("CreateMultipartUpload %s/%s without tags through %s", tagBucket, untagged, id), err))
	uploaded := id + "/upload"
	if err := tagUpload(ctx, client, uploaded, "Stage=Final", content); err != nil {
		errs = append(errs, fmt.Errorf("through %s: %w", id, err))
	} else {
		errs = append(errs, tagsAre(ctx, client, "multipart upload begun with Stage=Final through "+id, uploaded, map[string]string{"stage": "final"}))
	}

	// A copy that keeps its source's tags (the default directive, COPY) is
	// held to the rules of the bucket it is copied into: a source from a
	// bucket without rules, tagged outside the catalogue, is refused.
	kept := id + "/copy-kept"
	_, err = client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(tagBucket), Key: aws.String(kept), CopySource: aws.String(plainBucket + "/" + plainSource),
	})
	what := fmt.Sprintf("CopyObject of %s/%s to %s/%s keeping its tags through %s", plainBucket, plainSource, tagBucket, kept, id)
	if unexpected := mustRefuseTag(what, err); unexpected != nil {
		errs = append(errs, unexpected)
	} else {
		errs = append(errs, notStored(ctx, client, what, kept))
	}

	// What follows is about the draft put above; when its put got no
	// answer in this round there is nothing to copy or to tag again.
	if stored {
		source := aws.String(tagBucket + "/" + draft)
		copiedDraft := id + "/copy-draft"
		if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: aws.String(tagBucket), Key: aws.String(copiedDraft), CopySource: source,
		}); err != nil {
			errs = append(errs, fmt.Errorf("CopyObject to %s/%s keeping its tags through %s: %w", tagBucket, copiedDraft, id, err))
		} else {
			errs = append(errs, tagsAre(ctx, client, "copy keeping its tags through "+id, copiedDraft, map[string]string{"stage": "draft"}))
		}
		for _, copied := range []struct {
			name, tagging string
		}{
			{"copy-unstaged", "Owner=Albie"},
			{"copy-outside", "Stage=Final&Colour=Red"},
		} {
			key := id + "/" + copied.name
			_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket: aws.String(tagBucket), Key: aws.String(key), CopySource: source,
				TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String(copied.tagging),
			})
			what := fmt.Sprintf("CopyObject to %s/%s with REPLACE and tags %q through %s", tagBucket, key, copied.tagging, id)
			if unexpected := mustRefuseTag(what, err); unexpected != nil {
				errs = append(errs, unexpected)
				continue
			}
			errs = append(errs, notStored(ctx, client, what, key))
		}
		copied := id + "/copy"
		if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: aws.String(tagBucket), Key: aws.String(copied), CopySource: source,
			TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String("Stage=Final&Owner=Albie"),
		}); err != nil {
			errs = append(errs, fmt.Errorf("CopyObject to %s/%s with REPLACE and Stage=Final&Owner=Albie through %s: %w", tagBucket, copied, id, err))
		} else {
			errs = append(errs, tagsAre(ctx, client, "copy with REPLACE through "+id, copied, map[string]string{"stage": "final", "owner": "albie"}))
		}

		// The required tag cannot be taken away, by a new tag set without
		// it or by deleting the tags; the object keeps the tags it had.
		// A change that got no answer is looked for again next round.
		_, err := client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket: aws.String(tagBucket), Key: aws.String(draft), Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("Owner"), Value: aws.String("Albie")}}},
		})
		round.unsure = round.unsure || unanswered(sdkStatus(err))
		errs = append(errs, mustRefuseTag(fmt.Sprintf("PutObjectTagging %s/%s without stage through %s", tagBucket, draft, id), err))
		_, err = client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: aws.String(tagBucket), Key: aws.String(draft)})
		round.unsure = round.unsure || unanswered(sdkStatus(err))
		errs = append(errs, mustRefuseTag(fmt.Sprintf("DeleteObjectTagging %s/%s through %s", tagBucket, draft, id), err))
		errs = append(errs, tagsAre(ctx, client, "after its required tag was not taken away through "+id, draft, map[string]string{"stage": "draft"}))
	}

	// The definition a rule names stays in the catalogue.
	switch status, header, body, err := gridCall(ctx, c, id, root, http.MethodDelete, "/tag-definitions/stage", nil); {
	case status == http.StatusConflict && previewProblem(header, body) == "EntityInUse":
	case unanswered(status):
		errs = append(errs, errors.Join(fmt.Errorf("DELETE /tag-definitions/stage through %s: not answered yet (status %d)", id, status), err))
	default:
		errs = append(errs, errors.Join(fmt.Errorf("%w: DELETE /tag-definitions/stage through %s = %d %s, want 409 EntityInUse", errWrongAnswer, id, status, body), err))
	}
	switch status, _, body, err := gridCall(ctx, c, id, root, http.MethodGet, "/tag-definitions/stage", nil); {
	case err != nil:
		errs = append(errs, err)
	case status == http.StatusNotFound:
		errs = append(errs, fmt.Errorf("%w: GET /tag-definitions/stage through %s = 404: the definition a rule names was deleted", errWrongAnswer, id))
	case status != http.StatusOK:
		errs = append(errs, fmt.Errorf("GET /tag-definitions/stage through %s = %d %s", id, status, body))
	}
	return errors.Join(errs...)
}

// TestTagRulesAcrossTheCluster: tag rules and the catalogue of tags in the
// product as it is deployed (ADR-0018, PLAN D.5). Through the management API
// root makes a bucket, which lower-cases its tags because it was made there;
// an object put with `Stage=Draft` through another node reads back as
// `stage=draft`. Root then defines `stage` (draft or final) and `owner` in the
// catalogue and sets rules that take only tags of the catalogue and require
// `stage`; with S3's CreateBucket it makes a bucket without rules holding an
// object tagged `Colour=Red`. With aws-sdk-go-v2 through another node:
// PutObject (two keys that are one in lower case included), CopyObject -
// keeping the source's tags or replacing them -, CreateMultipartUpload and
// PutObjectTagging are refused 400 InvalidTag wherever the tags break a rule,
// and nothing is stored (404 NoSuchKey); those that keep them are stored in
// lower case, a multipart upload with the tags it was begun with;
// DeleteObjectTagging is refused. The management API's upload without the
// required tag is refused 400 TagRuleViolated, and with it stored in lower
// case; and the definition the rule names cannot be deleted (409
// EntityInUse). The answers are the same through another node once the
// leader of the metadata group has been stopped and another elected.
func TestTagRulesAcrossTheCluster(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	c := clustertest.Start(t, clustertest.Options{})
	root := gridBearer(c.AccessKey, c.SecretKey)

	mustAdmin := func(id string, want int, method, path string, body any) []byte {
		t.Helper()
		status, _, answer, err := gridCall(ctx, c, id, root, method, path, body)
		if err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Fatalf("%s %s through %s = %d %s, want %d", method, path, id, status, answer, want)
		}
		return answer
	}
	type rules struct {
		Lowercase     bool `json:"lowercase"`
		CatalogueOnly bool `json:"catalogue_only"`
		Required      []struct {
			Key    string   `json:"key"`
			Values []string `json:"values"`
		} `json:"required"`
	}

	// Made through the management API, the bucket lower-cases its tags
	// and asks nothing else of them.
	mustAdmin("node1", http.StatusCreated, http.MethodPost, "/buckets", map[string]any{"name": tagBucket})
	var made rules
	if answer := mustAdmin("node2", http.StatusOK, http.MethodGet, "/buckets/"+tagBucket+"/tag-rules", nil); json.Unmarshal(answer, &made) != nil ||
		!made.Lowercase || made.CatalogueOnly || len(made.Required) != 0 {
		t.Fatalf("the tag rules of a bucket made through the management API = %s, want lowercase alone", answer)
	}
	sdk := c.S3("node2")
	if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(tagBucket), Key: aws.String("before-the-rules"), Body: bytes.NewReader(randomBytes(t, 1024)), Tagging: aws.String("Stage=Draft"),
	}); err != nil {
		t.Fatalf("PutObject with Stage=Draft through node2: %v", err)
	}
	if err := tagsAre(ctx, sdk, "put with Stage=Draft through node2", "before-the-rules", map[string]string{"stage": "draft"}); err != nil {
		t.Fatal(err)
	}

	// A bucket made with S3's CreateBucket has no rules: its object keeps
	// a tag no catalogue defines, in the case it was sent in.
	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(plainBucket)}); err != nil {
		t.Fatalf("CreateBucket %s through node1: %v", plainBucket, err)
	}
	if _, err := c.S3("node1").PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(plainBucket), Key: aws.String(plainSource), Body: bytes.NewReader(randomBytes(t, 1024)), Tagging: aws.String("Colour=Red"),
	}); err != nil {
		t.Fatalf("PutObject %s/%s with Colour=Red through node1: %v", plainBucket, plainSource, err)
	}

	// The catalogue, then rules that hold the bucket to it.
	mustAdmin("node1", http.StatusCreated, http.MethodPost, "/tag-definitions", map[string]any{
		"key": "stage", "description": "Where the object is in its life", "values": []string{"draft", "final"},
	})
	mustAdmin("node1", http.StatusCreated, http.MethodPost, "/tag-definitions", map[string]any{
		"key": "owner", "description": "Who answers for the object",
	})
	mustAdmin("node1", http.StatusOK, http.MethodPut, "/buckets/"+tagBucket+"/tag-rules", map[string]any{
		"lowercase": true, "catalogue_only": true,
		"required": []any{map[string]any{"key": "stage", "values": []string{"draft", "final"}}},
	})
	var set rules
	if answer := mustAdmin("node2", http.StatusOK, http.MethodGet, "/buckets/"+tagBucket+"/tag-rules", nil); json.Unmarshal(answer, &set) != nil ||
		!set.Lowercase || !set.CatalogueOnly || len(set.Required) != 1 || set.Required[0].Key != "stage" {
		t.Fatalf("the tag rules through node2 = %s", answer)
	}

	content := randomBytes(t, 4096)
	// Through a node other than the one that took the changes.
	if err := tagRuleAnswers(ctx, c, "node2", root, sdk, &tagRound{}, content); err != nil {
		t.Fatal(err)
	}

	// The leader is stopped, as an orchestrator stops it; another is
	// elected, and a node that is neither answers as before.
	leader, err := askLeader(ctx, c, "node2")
	if err != nil {
		t.Fatal(err)
	}
	other := "node3"
	if leader == other {
		other = "node4"
	}
	c.Stop(leader)
	c.Eventually("a leader other than "+leader, 2*time.Minute, func() bool {
		now, err := askLeader(ctx, c, other)
		return err == nil && now != leader
	})
	sdk = c.S3(other)
	// Each missing answer is told once, so that a wait that times out says
	// what was missing. An answer that is wrong - above all one that stores
	// what the rules forbid - fails the test at once: only no answer is
	// waited out.
	told := ""
	round := &tagRound{}
	c.Eventually("the tag rules' answers through "+other+" without "+leader, 2*time.Minute, func() bool {
		err := tagRuleAnswers(ctx, c, other, root, sdk, round, content)
		if errors.Is(err, errWrongAnswer) {
			t.Fatalf("through %s without %s: %v", other, leader, err)
		}
		if err != nil && err.Error() != told {
			told = err.Error()
			t.Logf("not yet: %v", err)
		}
		return err == nil
	})
}
