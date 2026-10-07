// Package s3compat is the S3 compatibility suite: the contract of the CactusFS
// data API. Tests are compiled only with -tags=s3compat and are run by
// "make s3-compat", which fails when a test recorded in baseline.txt no longer
// passes.
//
// The suite is empty until the S3 API exists (PLAN 1.10).
package s3compat
