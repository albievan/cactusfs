//go:build tools

// Command trustedroot refreshes the trusted root of Sigstore's public-good
// instance that the release verifier embeds
// (internal/adapters/releases/sigstore/trusted_root.json; PLAN P.4,
// ADR-0029). It is not part of the product: the build tag `tools` keeps it
// out of every build and gate, and `make trusted-root` runs it.
//
// The file is not downloaded as it is: it is a target of Sigstore's TUF
// repository, fetched with sigstore-go's TUF client, which starts from the
// TUF root that sigstore-go embeds, walks the chain of signed root metadata
// to the current one, and hands out a target only when its length and hashes
// are the ones the signed targets metadata names. The result is parsed as a
// trusted root before it is written, and written atomically. Nothing is
// cached outside the repository.
//
//	go run -tags tools ./internal/tools/trustedroot [-out <file>]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
)

// target is the name of the trusted root in Sigstore's TUF repository.
const target = "trusted_root.json"

func main() {
	out := flag.String("out", "internal/adapters/releases/sigstore/trusted_root.json", "the file to write")
	timeout := flag.Duration("timeout", 2*time.Minute, "how long the TUF repository has to answer")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: trustedroot [-out <file>] [-timeout <duration>]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	err := run(ctx, *out)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "trusted-root:", err)
		os.Exit(1)
	}
}

// run fetches the trusted root through TUF and writes it to out.
func run(ctx context.Context, out string) error {
	opts := tuf.DefaultOptions().
		WithRoot(tuf.DefaultRoot()).
		WithRepositoryBaseURL(tuf.DefaultMirror).
		WithDisableLocalCache().
		WithContext(ctx)
	client, err := tuf.New(opts)
	if err != nil {
		return fmt.Errorf("the TUF client of %s: %w", tuf.DefaultMirror, err)
	}
	content, err := client.GetTarget(target)
	if err != nil {
		return fmt.Errorf("the target %s of %s: %w", target, tuf.DefaultMirror, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parsed, err := root.NewTrustedRootFromJSON(content)
	if err != nil {
		return fmt.Errorf("%s is not a trusted root: %w", target, err)
	}
	if len(parsed.FulcioCertificateAuthorities()) == 0 || len(parsed.RekorLogs()) == 0 || len(parsed.CTLogs()) == 0 {
		return errors.New(target + " names no certificate authority, transparency log or CT log")
	}
	if err := writeAtomically(out, content); err != nil {
		return err
	}
	fmt.Printf("trusted-root: wrote %s (%d bytes) from %s\n", out, len(content), tuf.DefaultMirror)
	return nil
}

// writeAtomically writes content to a temporary file beside name, syncs it
// and renames it into place.
func writeAtomically(name string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".trusted_root-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}
