package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/fsutil"
	"github.com/albievan/cactusfs/internal/adapters/kms/local"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// Files "cactusfs dev-init" writes into its directory.
const (
	devNodeCAFile  = "node-ca.pem"
	devNodesDir    = "nodes"
	devS3CAFile    = "s3-ca.pem"
	devS3CertFile  = "s3.pem"
	devS3KeyFile   = "s3.key"
	devKMSFile     = "kms.json"
	devReadyFile   = "ready"
	devDirMode     = 0o700
	devPublicMode  = 0o644
	devPrivateMode = 0o600

	defaultDevValidity = 365 * 24 * time.Hour
)

const devInitUsage = `Usage: cactusfs dev-init --dir <directory> --nodes <id>[,<id>...] [flags]

Writes what the nodes of a DEVELOPMENT cluster must share before they can
start: a throw-away CA for node certificates and a certificate and key for
every node, a second throw-away CA with a certificate for the S3 and admin
listeners, and one sealed KMS key file. The passphrase of the key file is
read from $` + envKMSPhrase + ` or --kms-passphrase-file.

The private keys of the two CAs are not kept: nothing more can be issued
later. A directory that is complete is left alone, so the command can run
before every start. A production cluster brings certificates from its own
PKI and its keys from Vault.

`

// runDevInit implements "cactusfs dev-init".
func runDevInit(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("cactusfs dev-init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, devInitUsage)
		fs.PrintDefaults()
	}
	dir := fs.String("dir", "", "`directory` to write into; created when missing")
	var nodes, hosts stringList
	fs.Var(&nodes, "nodes", "node `IDs` to issue certificates for, separated by commas")
	fs.Var(&hosts, "hosts", "extra DNS `names` or IP addresses clients use to reach the S3 API; "+
		"localhost, the loopback addresses and the node IDs are always included")
	validity := fs.Duration("validity", defaultDevValidity, "how long the certificates are valid")
	passphraseFile := fs.String("kms-passphrase-file", "", "`path` of a file holding the KMS passphrase, instead of $"+envKMSPhrase)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	cfg := serverConfig{KMSPassphraseFile: *passphraseFile, secrets: secrets{kmsPassphrase: getenv(envKMSPhrase)}}
	err := func() error {
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		if *dir == "" || len(nodes) == 0 {
			return errors.New("--dir and --nodes are required")
		}
		if *validity <= 0 {
			return errors.New("--validity must be positive")
		}
		return cfg.validateSecrets()
	}()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cactusfs dev-init: %v\n", err)
		return exitUsage
	}
	passphrase, err := readPassphrase(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cactusfs dev-init: %v\n", err)
		return exitError
	}
	defer clear(passphrase)

	written, err := devInit(*dir, nodes, hosts, passphrase, *validity, time.Now())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cactusfs dev-init: %v\n", err)
		return exitError
	}
	if !written {
		_, _ = fmt.Fprintf(stdout, "dev-init: %s is complete; nothing written\n", *dir)
		return exitOK
	}
	_, _ = fmt.Fprintf(stdout, "dev-init: wrote development certificates for %d nodes and a KMS key file to %s\n", len(nodes), *dir)
	return exitOK
}

// devInit writes the files of a development cluster into dir. It reports
// false, and writes nothing, when the directory is complete already.
//
// Nothing is ever overwritten or removed: the KMS key file in a directory is
// the only copy of the key that every object of that cluster is encrypted
// under.
func devInit(dir string, nodes, hosts []string, passphrase []byte, validity time.Duration, now time.Time) (bool, error) {
	seen := map[string]bool{}
	for _, id := range nodes {
		if err := cluster.ValidateNodeID(id); err != nil {
			return false, err
		}
		if seen[id] {
			return false, fmt.Errorf("node %s is listed twice", id)
		}
		seen[id] = true
	}
	if err := os.MkdirAll(dir, devDirMode); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()

	switch _, err := root.Lstat(devReadyFile); {
	case err == nil:
		return false, nil
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(entries) > 0 {
		return false, fmt.Errorf("%s is not empty and not complete: an earlier run was interrupted, or the directory "+
			"is something else; nothing in it is touched - give an empty directory", dir)
	}

	// Everything is made before anything is written.
	type file struct {
		name string
		data []byte
		mode os.FileMode
	}
	nodeCA, err := nodetls.NewAuthority(rand.Reader, now, validity)
	if err != nil {
		return false, err
	}
	files := []file{{devNodeCAFile, nodeCA.CertPEM, devPublicMode}}
	names := []string{"localhost"}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, id := range nodes {
		certPEM, keyPEM, err := nodeCA.Issue(id, now)
		if err != nil {
			return false, err
		}
		files = append(files,
			file{devNodesDir + "/" + id + ".pem", certPEM, devPublicMode},
			file{devNodesDir + "/" + id + ".key", keyPEM, devPrivateMode})
		names = append(names, id)
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
			continue
		}
		names = append(names, host)
	}
	s3CA, err := nodetls.NewNamedAuthority(rand.Reader, now, validity, "CactusFS S3 CA (development)")
	if err != nil {
		return false, err
	}
	s3Cert, s3Key, err := s3CA.IssueServer(names, ips, now)
	if err != nil {
		return false, err
	}
	files = append(files,
		file{devS3CAFile, s3CA.CertPEM, devPublicMode},
		file{devS3CertFile, s3Cert, devPublicMode},
		file{devS3KeyFile, s3Key, devPrivateMode})

	if err := root.Mkdir(devNodesDir, devDirMode); err != nil {
		return false, err
	}
	for _, f := range files {
		if err := fsutil.WriteNewDurable(root, f.name, f.data, f.mode); err != nil {
			return false, err
		}
	}
	if err := local.Create(filepath.Join(dir, devKMSFile), passphrase, local.DefaultParams(), rand.Reader); err != nil {
		return false, err
	}
	// The marker is last: a directory without it was not finished.
	if err := fsutil.WriteNewDurable(root, devReadyFile, []byte(now.UTC().Format(time.RFC3339)+"\n"), devPublicMode); err != nil {
		return false, err
	}
	return true, nil
}
