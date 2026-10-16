package preview_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/preview"
	"github.com/albievan/cactusfs/internal/domain/preview/previewtest"
)

// judge decides a preview of an object as the use case does, from the
// domain alone: its stored type before any byte, then its first bytes, an
// image's size, and a GIF's frames to its end. byType says that the stored
// type refused it.
func judge(storedType string, data []byte) (kind preview.Kind, byType bool, err error) {
	if _, err := preview.Expected(storedType); err != nil {
		return "", true, err
	}
	kind, err = preview.Classify(storedType, data[:min(len(data), preview.HeadSize)])
	if err != nil {
		return "", false, err
	}
	if kind.Image() {
		if _, err := preview.ImageBounds(kind, data[:min(len(data), preview.BoundsHeadSize)]); err != nil {
			return "", false, err
		}
	}
	if kind == preview.KindGIF {
		if _, err := preview.NewGIFFrames().Write(data); err != nil {
			return "", false, fmt.Errorf("the frames: %w", err)
		}
	}
	return kind, false, nil
}

// TestMaliciousFiles: every file of the malicious-file suite (PLAN O.11)
// is refused with its error - by its type alone where the case says so -
// or shown as the kind its bytes prove, and as nothing else.
func TestMaliciousFiles(t *testing.T) {
	t.Parallel()
	for _, c := range previewtest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			kind, byType, err := judge(c.StoredType, c.Data)
			if c.Refusal != nil {
				if !errors.Is(err, c.Refusal) || kind != "" || byType != c.ByType {
					t.Fatalf("judged %q (by its type: %v), %v; want %v (by its type: %v)", kind, byType, err, c.Refusal, c.ByType)
				}
				// The other refusal is not also said.
				for _, other := range []error{domain.ErrNotPreviewable, domain.ErrPreviewTooLarge} {
					if !errors.Is(c.Refusal, other) && errors.Is(err, other) {
						t.Fatalf("refused with %v and %v", err, other)
					}
				}
				return
			}
			if err != nil || kind != c.Kind {
				t.Fatalf("judged %q, %v; want %q", kind, err, c.Kind)
			}
		})
	}
}
