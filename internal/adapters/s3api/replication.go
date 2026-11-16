package s3api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// Replication configuration: the ?replication sub-resource of a bucket
// (PutBucketReplication, GetBucketReplication and DeleteBucketReplication of
// the S3 API reference; ADR-0026, "Configuration"). The role ARN names the
// target of the deployment the versions are sent through - there is no role
// to assume - and the destination names the bucket there. Whether the target
// may be named, and whether the bucket may be replicated, is the use case's
// to decide; what is read here is what the document says, strictly.

// Replications is the use case of the replication configuration of buckets.
// *app.ReplicationService implements it.
type Replications interface {
	Configuration(ctx context.Context, principal iam.Principal, bucketName string) (bucket.ReplicationConfig, error)
	SetConfiguration(ctx context.Context, principal iam.Principal, bucketName string, config bucket.ReplicationConfig) (bucket.ReplicationConfig, error)
	DeleteConfiguration(ctx context.Context, principal iam.Principal, bucketName string) error
}

const (
	// replicationRolePrefix is what the role ARN of a configuration begins
	// with; the name of the target follows it.
	replicationRolePrefix = "arn:cactusfs:replication:::"
	// destinationPrefix is what the ARN of the bucket at the other
	// deployment begins with; its name follows it.
	destinationPrefix = "arn:aws:s3:::"
	// The two values of a Status.
	statusEnabled  = "Enabled"
	statusDisabled = "Disabled"
)

// errNoReplication is the answer of GET for a bucket without a
// configuration, as S3 gives it.
var errNoReplication = errNoSuchConfig("ReplicationConfigurationNotFoundError", "replication configuration")

// replicationConfiguration is S3's ReplicationConfiguration as it is read.
// Every element that may be left out is a pointer, so that one left out is
// told from one that is empty.
type replicationConfiguration struct {
	XMLName xml.Name             `xml:"ReplicationConfiguration"`
	Role    *string              `xml:"Role"`
	Rules   []replicationRuleXML `xml:"Rule"`
}

type replicationRuleXML struct {
	ID       *string `xml:"ID"`
	Status   string  `xml:"Status"`
	Priority *string `xml:"Priority"`
	// Prefix is where a rule of the first version of the schema keeps its
	// prefix; Filter is where a later one does. Both are read; Filter is
	// written.
	Prefix *string `xml:"Prefix"`
	Filter *struct {
		Prefix *string `xml:"Prefix"`
	} `xml:"Filter"`
	DeleteMarkerReplication *struct {
		Status string `xml:"Status"`
	} `xml:"DeleteMarkerReplication"`
	Destination *struct {
		Bucket *string `xml:"Bucket"`
	} `xml:"Destination"`
}

// replicationShape is where each element of a replication configuration
// belongs.
func replicationShape() xmlShape {
	return xmlShape{
		"":                         {"ReplicationConfiguration"},
		"ReplicationConfiguration": {"Role", "Rule*"},
		"Rule":                     {"ID", "Status", "Priority", "Prefix", "Filter", "DeleteMarkerReplication", "Destination"},
		"Filter":                   {"Prefix"},
		"DeleteMarkerReplication":  {"Status"},
		"Destination":              {"Bucket"},
	}
}

// unsupportedReplication are the elements of S3's replication configuration
// that ask for what is not implemented: filters on tags, replication of
// objects encrypted with AWS KMS keys or of what was there before, another
// storage class, encryption, account or owner at the destination, and the
// time and metrics of AWS's own. A configuration that has one, wherever it
// is, is refused as such: read without it, the rule would send other
// versions, or send them otherwise, than was meant.
func unsupportedReplication() []string {
	return []string{
		"Tag", "And", "SourceSelectionCriteria", "ExistingObjectReplication",
		"StorageClass", "EncryptionConfiguration", "ReplicationTime", "Metrics", "Account", "AccessControlTranslation",
	}
}

func invalidReplication(format string, args ...any) error {
	return fmt.Errorf("%w: replication configuration: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// replicationPriority reads a Priority: decimal digits without a sign, a
// space or a leading zero, from 0 to 2^31 - 1.
func replicationPriority(text string) (int, bool) {
	if text == "" || len(text) > 10 || (len(text) > 1 && text[0] == '0') {
		return 0, false
	}
	for i := range len(text) {
		if text[i] < '0' || text[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

// statusOf reads the Status of a rule or of its delete markers: Enabled or
// Disabled, exactly. Anything else is a document that does not say what the
// schema lets it say.
func statusOf(status string) (bool, error) {
	switch status {
	case statusEnabled:
		return true, nil
	case statusDisabled:
		return false, nil
	}
	return false, errMalformedXML
}

// replicationRules reads a ReplicationConfiguration as the configuration it
// says. What the schema requires - a role, a rule, each rule's status and
// destination bucket, which keys it is for, and in the later form of the
// schema what becomes of delete markers - is malformed when it is missing,
// and so is a document whose rules are not all of one form: the first
// (`<Prefix>` in the rule) or the later (`<Filter>`). A role ARN or a
// destination ARN of another form, a priority that is no number of its
// range, an ID that is there and empty, and a rule with both a Prefix and a
// Filter are invalid arguments; rules that name different destination
// buckets are not implemented (one destination a bucket). A rule's ID left
// out, the target, rules that tie, and everything the domain judges are the
// use case's.
func replicationRules(in replicationConfiguration) (bucket.ReplicationConfig, error) {
	if in.Role == nil || len(in.Rules) == 0 {
		return bucket.ReplicationConfig{}, errMalformedXML
	}
	laterForm := in.Rules[0].Filter != nil
	target, ok := strings.CutPrefix(*in.Role, replicationRolePrefix)
	if !ok {
		return bucket.ReplicationConfig{}, invalidReplication("the role names a target as %s<target>", replicationRolePrefix)
	}
	config := bucket.ReplicationConfig{Target: target}
	for i, r := range in.Rules {
		if r.Destination == nil || r.Destination.Bucket == nil {
			return bucket.ReplicationConfig{}, errMalformedXML
		}
		switch {
		case r.Prefix != nil && r.Filter != nil:
			return bucket.ReplicationConfig{}, invalidReplication("rule %d: Prefix and Filter exclude each other", i)
		case r.Prefix == nil && r.Filter == nil:
			// A rule that says not which keys it is for would be read as
			// a rule for every key.
			return bucket.ReplicationConfig{}, errMalformedXML
		case (r.Filter != nil) != laterForm:
			// The two forms of the schema differ in what a rule that says
			// nothing of delete markers does: one document has one form.
			return bucket.ReplicationConfig{}, errMalformedXML
		case laterForm && r.DeleteMarkerReplication == nil:
			// The later form says what becomes of delete markers, as S3
			// requires of it.
			return bucket.ReplicationConfig{}, errMalformedXML
		}
		enabled, err := statusOf(r.Status)
		if err != nil {
			return bucket.ReplicationConfig{}, err
		}
		rule := bucket.ReplicationRule{Enabled: enabled}
		if r.DeleteMarkerReplication != nil {
			// A rule of the first form that says nothing of delete
			// markers does not copy them: S3 would, and a marker that
			// crosses can hide a version at the destination (ADR-0026).
			if rule.Markers, err = statusOf(r.DeleteMarkerReplication.Status); err != nil {
				return bucket.ReplicationConfig{}, err
			}
		}
		destination, ok := strings.CutPrefix(*r.Destination.Bucket, destinationPrefix)
		switch {
		case !ok:
			return bucket.ReplicationConfig{}, invalidReplication("rule %d: the destination names a bucket as %s<bucket>", i, destinationPrefix)
		case i == 0:
			config.Bucket = destination
		case destination != config.Bucket:
			// One target and one destination bucket a source bucket.
			return bucket.ReplicationConfig{}, errNotImplemented()
		}
		if r.ID != nil {
			if *r.ID == "" {
				return bucket.ReplicationConfig{}, invalidReplication("rule %d: an ID of nothing", i)
			}
			rule.ID = *r.ID
		}
		if r.Priority != nil {
			if rule.Priority, ok = replicationPriority(*r.Priority); !ok {
				return bucket.ReplicationConfig{}, invalidReplication("rule %d: a priority from 0 to %d", i, math.MaxInt32)
			}
		}
		switch {
		case r.Prefix != nil:
			rule.Prefix = *r.Prefix
		case r.Filter.Prefix != nil:
			rule.Prefix = *r.Filter.Prefix
		}
		config.Rules = append(config.Rules, rule)
	}
	return config, nil
}

// replicationConfigurationXML is a configuration as GET writes it: every
// rule in the later form of the schema, with its Filter and its priority,
// in the order the rules were put.
type replicationConfigurationXML struct {
	XMLName xml.Name                `xml:"ReplicationConfiguration"`
	XMLNS   string                  `xml:"xmlns,attr"`
	Role    string                  `xml:"Role"`
	Rules   []replicationRuleOutXML `xml:"Rule"`
}

type replicationRuleOutXML struct {
	ID       string `xml:"ID"`
	Status   string `xml:"Status"`
	Priority int    `xml:"Priority"`
	Filter   struct {
		Prefix string `xml:"Prefix"`
	} `xml:"Filter"`
	DeleteMarkerReplication struct {
		Status string `xml:"Status"`
	} `xml:"DeleteMarkerReplication"`
	Destination struct {
		Bucket string `xml:"Bucket"`
	} `xml:"Destination"`
}

// statusText is the Status that says on.
func statusText(on bool) string {
	if on {
		return statusEnabled
	}
	return statusDisabled
}

// replicationConfigurationOf is a configuration as S3 shows it.
func replicationConfigurationOf(config bucket.ReplicationConfig) replicationConfigurationXML {
	out := replicationConfigurationXML{XMLNS: s3Namespace, Role: replicationRolePrefix + config.Target}
	for _, r := range config.Rules {
		rule := replicationRuleOutXML{ID: r.ID, Status: statusText(r.Enabled), Priority: r.Priority}
		rule.Filter.Prefix = r.Prefix
		rule.DeleteMarkerReplication.Status = statusText(r.Markers)
		rule.Destination.Bucket = destinationPrefix + config.Bucket
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// replications is the use case of replication configuration, or the answer
// of a node that was not given it.
func (h *Handler) replications() (Replications, error) {
	if h.cfg.Replication == nil {
		return nil, errNotImplemented()
	}
	return h.cfg.Replication, nil
}

func (h *Handler) putBucketReplication(ctx context.Context, req *request) error {
	replications, err := h.replications()
	if err != nil {
		return err
	}
	raw, err := h.configBody(req)
	if err != nil {
		return err
	}
	var in replicationConfiguration
	if err := decodeShapedXML(bytes.NewReader(raw), maxConfigBody, &in, replicationShape(), unsupportedReplication()...); err != nil {
		return err
	}
	config, err := replicationRules(in)
	if err != nil {
		return err
	}
	if _, err := replications.SetConfiguration(ctx, req.principal, req.bucket, config); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getBucketReplication(ctx context.Context, req *request) error {
	replications, err := h.replications()
	if err != nil {
		return err
	}
	config, err := replications.Configuration(ctx, req.principal, req.bucket)
	if err != nil {
		return err
	}
	if config.Empty() {
		return errNoReplication
	}
	h.writeXML(ctx, req, http.StatusOK, replicationConfigurationOf(config))
	return nil
}

func (h *Handler) deleteBucketReplication(ctx context.Context, req *request) error {
	replications, err := h.replications()
	if err != nil {
		return err
	}
	if err := replications.DeleteConfiguration(ctx, req.principal, req.bucket); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}
