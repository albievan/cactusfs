package app

import (
	"context"
)

// CountObjects is the walk behind Usage, with a bound a test can reach.
func (s *ObjectService) CountObjects(ctx context.Context, bucketName string, limit int) (BucketUsage, error) {
	return s.countObjects(ctx, bucketName, limit)
}
