package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/tagging"
)

// Tag rules (ADR-0018). The rules of a bucket are read once a request,
// strongly consistent, as the bucket's other configuration is; the tags the
// request gives are then kept as the rules say - in lower case where they
// lower-case - and checked against them before anything is written. A put
// that races a change of the rules may be judged by the rules before.

// readTagRules reads the tag rules of a bucket; a bucket without rules has
// rules that ask for nothing. Rules this release cannot read are not
// guessed at: a document of a later release is domain.ErrUnavailable (the
// node is to be upgraded, the request can be repeated through another),
// one that does not parse is domain.ErrIntegrity. Either way nothing is
// written that the rules might not allow.
func readTagRules(ctx context.Context, buckets BucketPolicySource, name string) (bucket.TagRules, error) {
	raw, err := buckets.GetBucketConfig(ctx, name, bucket.ConfigTagRules)
	if err != nil {
		return bucket.TagRules{}, err
	}
	rules, err := bucket.ParseTagRules(raw)
	switch {
	case errors.Is(err, domain.ErrUnsupportedFormat):
		return bucket.TagRules{}, fmt.Errorf("%w: the tag rules of bucket %s are of a later release: %s", domain.ErrUnavailable, name, err.Error())
	case err != nil:
		return bucket.TagRules{}, fmt.Errorf("%w: stored tag rules of bucket %s: %s", domain.ErrIntegrity, name, err.Error())
	}
	return rules, nil
}

// admitTags decides the tags a new version of resource is to have, before a
// byte of it is written: whoever gives tags must be allowed to tag them
// (s3:PutObjectTagging), and the bucket's rules, read once, keep them and
// are checked - no tags too, where a tag is required of the key.
func (s *ObjectService) admitTags(ctx context.Context, principal iam.Principal, resource iam.Resource, tags map[string]string) (map[string]string, error) {
	if len(tags) > 0 {
		if err := object.ValidateTags(tags, object.MaxObjectTags); err != nil {
			return nil, err
		}
		if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionPutObjectTagging, Resource: resource}); err != nil {
			return nil, err
		}
	}
	rules, err := readTagRules(ctx, s.buckets, resource.Bucket)
	if err != nil {
		return nil, err
	}
	return s.keepObjectTags(ctx, rules, resource.Key, tags)
}

// keepObjectTags returns the tags of an object as the bucket's rules keep
// them - lower-cased where the rules say so - once they are known to keep
// the rules. The error wraps domain.ErrInvalidTag for tags that cannot be
// kept at all, domain.ErrTagRule for tags the rules do not allow, and
// domain.ErrUnavailable when the rules take only tags of the catalogue and
// the catalogue cannot be read.
func (s *ObjectService) keepObjectTags(ctx context.Context, rules bucket.TagRules, key string, tags map[string]string) (map[string]string, error) {
	kept, err := rules.Keep(tags, object.MaxObjectTags)
	if err != nil {
		return nil, err
	}
	if err := s.checkObjectTags(ctx, rules, key, kept); err != nil {
		return nil, err
	}
	return kept, nil
}

// checkObjectTags reports whether tags, as they are to be stored, keep the
// bucket's rules.
func (s *ObjectService) checkObjectTags(ctx context.Context, rules bucket.TagRules, key string, tags map[string]string) error {
	defined, err := s.catalogue(ctx, rules, tags)
	if err != nil {
		return err
	}
	return rules.Check(key, tags, defined)
}

// catalogue returns what the deployment's catalogue defines, when the rules
// take only tags of it and there are tags to look up; nil otherwise, which
// bucket.TagRules.Check then does not ask. A catalogue that cannot be read
// is domain.ErrUnavailable and never nil: a nil one would refuse the tags
// as if they broke a rule.
func (s *ObjectService) catalogue(ctx context.Context, rules bucket.TagRules, tags map[string]string) (bucket.Defined, error) {
	if !rules.CatalogueOnly || len(tags) == 0 {
		return nil, nil
	}
	if s.tagDefinitions == nil {
		return nil, fmt.Errorf("%w: the catalogue of tags cannot be read on this node", domain.ErrUnavailable)
	}
	definitions, err := s.tagDefinitions.ListTagDefinitions(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: the catalogue of tags could not be read: %s", domain.ErrUnavailable, err.Error())
	}
	return definedBy(definitions), nil
}

// definedBy answers bucket.Defined from the definitions of the catalogue.
func definedBy(definitions []tagging.Definition) bucket.Defined {
	byKey := make(map[string][]string, len(definitions))
	for _, d := range definitions {
		byKey[d.Key] = d.Values
	}
	return func(key string) ([]string, bool) {
		values, ok := byKey[key]
		return values, ok
	}
}
