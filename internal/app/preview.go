package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/preview"
)

// The bounds of previews (ADR-0028).
const (
	// DefaultPreviewMaxSize is the largest object previewed when the
	// deployment says nothing else; MinPreviewMaxSize and
	// MaxPreviewMaxSize bound what it may say.
	DefaultPreviewMaxSize = 32 << 20
	MinPreviewMaxSize     = 1 << 20
	MaxPreviewMaxSize     = 256 << 20
	// PreviewTextSize is how much of text, CSV and JSON a preview shows:
	// their beginning, whatever their size.
	PreviewTextSize = 1 << 20
	// PreviewsPerPrincipal and PreviewsPerNode are how many previews are
	// read at once, for one caller and on one node.
	PreviewsPerPrincipal = 2
	PreviewsPerNode      = 16
)

// previewLimiter counts the previews being read on this node, by who reads
// them.
type previewLimiter struct {
	mu      sync.Mutex
	running map[string]int
	total   int
}

// admit lets a preview begin, or says that there are too many. The
// returned function ends it; calling it more than once ends it once.
func (l *previewLimiter) admit(actor string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[actor] >= PreviewsPerPrincipal {
		return nil, fmt.Errorf("%w: %d previews of yours are still being read", domain.ErrTooManyRequests, PreviewsPerPrincipal)
	}
	if l.total >= PreviewsPerNode {
		return nil, fmt.Errorf("%w: this node is reading as many previews as it reads at once", domain.ErrTooManyRequests)
	}
	if l.running == nil {
		l.running = map[string]int{}
	}
	l.running[actor]++
	l.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.running[actor]--; l.running[actor] <= 0 {
				delete(l.running, actor)
			}
			l.total--
		})
	}, nil
}

// PreviewInput names the object version to preview.
type PreviewInput struct {
	Bucket string
	Key    string
	// VersionID names the version as clients name one - "null", or the ID
	// of a version; empty is the current version.
	VersionID string
}

// PreviewOutput is a preview being read.
type PreviewOutput struct {
	// Version is the version shown: the one that was judged.
	Version object.Version
	// Kind is what it is shown as.
	Kind preview.Kind
	// Length is how many bytes Body yields when it is read to its end:
	// the size of the version, or for the text kinds at most
	// PreviewTextSize. Truncated says that the version has more.
	Length    int64
	Truncated bool
	// Body yields the bytes. Every byte it returns has been authenticated,
	// as a download's are; it fails rather than end short. The caller must
	// close it.
	Body io.ReadCloser
}

// Preview opens an object version for the console to show (ADR-0028).
//
// The caller must be allowed s3:GetObject - s3:GetObjectVersion for a named
// version - and, on the version thus resolved, admin:PreviewObject; for the
// user who committed that version, the second is allowed unless a policy
// denies it (iam.Decide, with Request.ObjectCreator). Only then is anything
// said of the version: an object under SSE-C wraps
// domain.ErrCustomerKeyRequired; one of a type no preview has, or whose bytes
// are not of the kind its type says, domain.ErrNotPreviewable; one larger
// than the node's cap - but for the text kinds, of which the first
// PreviewTextSize bytes are shown - or an image of too many pixels,
// domain.ErrPreviewTooLarge. Size and type are judged before any shard is
// read; the kind from the first bytes, and an image's size from at most
// preview.BoundsHeadSize of them. The frames of a GIF are checked to its
// end as the body is read: the body fails at one that reaches past the
// screen.
//
// At most PreviewsPerPrincipal previews of one caller, and PreviewsPerNode
// of all, are read at once: beyond, domain.ErrTooManyRequests. A preview
// counts as a download, once, when its body is closed (ADR-0016).
func (s *ObjectService) Preview(ctx context.Context, principal iam.Principal, in PreviewInput) (PreviewOutput, error) {
	out, err := s.preview(ctx, principal, in)
	if err != nil {
		return PreviewOutput{}, fmt.Errorf("object.Preview: %w", err)
	}
	return out, nil
}

// PreviewMaxSize is the largest object, but for the text kinds, this
// service previews.
func (s *ObjectService) PreviewMaxSize() int64 {
	return s.previewMaxSize
}

func (s *ObjectService) preview(ctx context.Context, principal iam.Principal, in PreviewInput) (PreviewOutput, error) {
	var ref *object.VersionRef
	if in.VersionID != "" {
		parsed, err := object.ParseVersionRef(in.VersionID)
		if err != nil {
			return PreviewOutput{}, err
		}
		ref = &parsed
	}
	var lastData object.ID
	for attempt := 1; ; attempt++ {
		out, judged, err := s.previewOnce(ctx, principal, in, ref)
		// As for Get: when what was judged was replaced or deleted
		// underneath before it could be opened, the object is judged anew,
		// from the start - who made what is there now included.
		gone := errors.Is(err, domain.ErrShardNotFound) || errors.Is(err, domain.ErrIntegrity)
		if judged != nil && gone && attempt < getAttempts && judged.DataID != lastData {
			lastData = judged.DataID
			continue
		}
		return out, err
	}
}

// previewOnce judges the version a preview is of and opens it. Its place
// among the previews being read is taken once the version was judged, and
// given back by the body's Close - or here, whatever ends the call before
// the body is handed out, a panic included. judged is the version when it
// was judged and could then not be opened, which the caller may try again.
func (s *ObjectService) previewOnce(
	ctx context.Context, principal iam.Principal, in PreviewInput, ref *object.VersionRef,
) (out PreviewOutput, judged *object.Version, err error) {
	version, expected, err := s.previewTarget(ctx, principal, in.Bucket, in.Key, ref)
	if err != nil {
		return PreviewOutput{}, nil, err
	}
	release, err := s.previews.admit(actorOf(principal))
	if err != nil {
		return PreviewOutput{}, nil, err
	}
	handed := false
	defer func() {
		if !handed {
			release()
		}
	}()
	length, truncated := version.Size, false
	get := GetInput{Bucket: in.Bucket, Key: in.Key}
	if expected.Textual() && version.Size > PreviewTextSize {
		length, truncated = PreviewTextSize, true
		get.Range = &object.RangeSpec{First: 0, Last: PreviewTextSize - 1}
	}
	// Exactly the version that was judged: never "the current one" again,
	// which may be another by now.
	opened, err := s.open(ctx, version, get)
	if err != nil {
		return PreviewOutput{}, &version, err
	}
	kind, body, err := previewBodyOf(version.ContentType, opened.Body)
	if err != nil {
		_ = opened.Body.Close()
		return PreviewOutput{}, nil, err
	}
	closed := func() {
		release()
		if s.access != nil {
			s.access.Download(principal, version)
		}
	}
	handed = true
	return PreviewOutput{
		Version: version, Kind: kind, Length: length, Truncated: truncated,
		Body: &countedBody{ReadCloser: readCloser{Reader: body, Closer: opened.Body}, count: closed},
	}, nil, nil
}

// previewTarget resolves the version a preview is of and judges it, before
// any of its bytes are read: whether the caller may see it - as a download,
// then as a preview by assignment or ownership of that very version -
// whether its key is the deployment's, and whether its type and size let it
// be shown. It returns the kind its stored type names, or "" when the bytes
// are to say.
func (s *ObjectService) previewTarget(
	ctx context.Context, principal iam.Principal, bucketName, key string, ref *object.VersionRef,
) (object.Version, preview.Kind, error) {
	version, err := s.head(ctx, principal, bucketName, key, ref)
	if err != nil {
		return object.Version{}, "", err
	}
	// Who made the version comes from the version, never from the caller.
	if err := s.authz.Authorize(ctx, iam.Request{
		Principal: principal, Action: iam.ActionPreviewObject, Resource: iam.Resource{Bucket: bucketName, Key: key},
		ObjectCreator: version.CreatedBy,
	}); err != nil {
		return object.Version{}, "", err
	}
	if version.Encryption.Mode == object.SSEC {
		return object.Version{}, "", fmt.Errorf("%w: the object is encrypted with a customer key, which a preview never holds", domain.ErrCustomerKeyRequired)
	}
	expected, err := preview.Expected(version.ContentType)
	if err != nil {
		return object.Version{}, "", err
	}
	if !expected.Textual() && version.Size > s.previewMaxSize {
		return object.Version{}, "", fmt.Errorf("%w: the object is %d bytes; a preview is at most %d", domain.ErrPreviewTooLarge, version.Size, s.previewMaxSize)
	}
	return version, expected, nil
}

// previewBodyOf reads the first bytes of an object, decides from them and
// its stored type what it is shown as and, for an image, whether it is not
// too large to be; and returns the whole of what body yields, those first
// bytes included. A GIF's frames are checked to its end as it is read.
func previewBodyOf(storedType string, body io.Reader) (preview.Kind, io.Reader, error) {
	head, err := readHead(body, preview.HeadSize)
	if err != nil {
		return "", nil, err
	}
	kind, err := preview.Classify(storedType, head)
	if err != nil {
		return "", nil, err
	}
	if kind.Image() {
		if len(head) == preview.HeadSize {
			more, err := readHead(body, preview.BoundsHeadSize-preview.HeadSize)
			if err != nil {
				return "", nil, err
			}
			head = append(head, more...)
		}
		if _, err := preview.ImageBounds(kind, head); err != nil {
			return "", nil, err
		}
	}
	whole := io.MultiReader(bytes.NewReader(head), body)
	if kind == preview.KindGIF {
		whole = &checkedReader{reader: whole, check: preview.NewGIFFrames()}
	}
	return kind, whole, nil
}

// readHead reads up to n bytes; fewer only when the body ends.
func readHead(body io.Reader, n int) ([]byte, error) {
	head := make([]byte, n)
	got, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:got], nil
}

// checkedReader hands every byte it reads to a check, and fails - for good,
// and without the bytes of the read that the check refused - once the check
// fails.
type checkedReader struct {
	reader io.Reader
	check  io.Writer
	err    error
}

// Read implements io.Reader.
func (r *checkedReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		if _, failed := r.check.Write(p[:n]); failed != nil {
			r.err = fmt.Errorf("object.Preview: %w", failed)
			return 0, r.err
		}
	}
	return n, err
}

// readCloser is a reader closed by another.
type readCloser struct {
	io.Reader
	io.Closer
}
