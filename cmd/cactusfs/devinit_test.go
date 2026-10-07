package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/kms/local"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
)

func readTestFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDevInit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "config")
	env := envMap(map[string]string{envKMSPhrase: nodePassphrase})
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := runDevInit(args, &stdout, &stderr, env)
		return code, stdout.String(), stderr.String()
	}

	code, stdout, stderr := run("--dir", dir, "--nodes", "node1,node2,node3", "--hosts", "s3.example.test,10.1.2.3")
	if code != exitOK || !strings.Contains(stdout, "3 nodes") {
		t.Fatalf("dev-init: exit %d, %q, %q", code, stdout, stderr)
	}

	// Every node can load its identity, and it names the node.
	for _, id := range []string{"node1", "node2", "node3"} {
		identity, err := nodetls.Load(filepath.Join(dir, "nodes", id+".pem"), filepath.Join(dir, "nodes", id+".key"), filepath.Join(dir, devNodeCAFile))
		if err != nil || identity.NodeID() != id {
			t.Errorf("node %s: %v", id, err)
		}
		info, err := os.Stat(filepath.Join(dir, "nodes", id+".key"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("key of %s: mode %v, %v", id, info.Mode().Perm(), err)
		}
	}
	// The key file opens with the passphrase, and only with it.
	if _, err := local.Open(filepath.Join(dir, devKMSFile), []byte(nodePassphrase)); err != nil {
		t.Errorf("KMS key file: %v", err)
	}
	if _, err := local.Open(filepath.Join(dir, devKMSFile), []byte("another passphrase entirely")); err == nil {
		t.Error("KMS key file opened with the wrong passphrase")
	}

	// The certificate of the S3 listener is for clients: it is valid under
	// its own CA for the names clients use, and it is NOT a certificate of
	// the node CA - whatever that CA signs is a node of the cluster.
	pair, err := tls.X509KeyPair(readTestFile(t, dir, devS3CertFile), readTestFile(t, dir, devS3KeyFile))
	if err != nil {
		t.Fatalf("S3 certificate: %v", err)
	}
	pool := func(name string) *x509.CertPool {
		p := x509.NewCertPool()
		if !p.AppendCertsFromPEM(readTestFile(t, dir, name)) {
			t.Fatalf("%s holds no certificate", name)
		}
		return p
	}
	for _, name := range []string{"localhost", "127.0.0.1", "node2", "s3.example.test", "10.1.2.3"} {
		if _, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: pool(devS3CAFile), DNSName: name}); err != nil {
			t.Errorf("S3 certificate for %s: %v", name, err)
		}
	}
	if _, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: pool(devNodeCAFile), DNSName: "localhost"}); err == nil {
		t.Error("the S3 certificate is signed by the node CA")
	}
	if _, err := pair.Leaf.Verify(x509.VerifyOptions{
		Roots: pool(devS3CAFile), DNSName: "localhost", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Error("the S3 certificate can be used as a client certificate")
	}
	// No CA key is kept: nothing in the directory can sign.
	entries, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, path := range entries {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		data := readTestFile(t, dir, filepath.Base(path))
		for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
			if strings.Contains(block.Type, "PRIVATE KEY") && filepath.Base(path) != devS3KeyFile {
				t.Errorf("%s holds a private key", path)
			}
		}
	}

	// A second run changes nothing: the key file is the only copy of the
	// cluster's key.
	before := readTestFile(t, dir, devKMSFile)
	code, stdout, _ = run("--dir", dir, "--nodes", "node1,node2,node3,node4")
	if code != exitOK || !strings.Contains(stdout, "nothing written") {
		t.Errorf("second dev-init: exit %d, %q", code, stdout)
	}
	if !bytes.Equal(readTestFile(t, dir, devKMSFile), before) {
		t.Fatal("a second dev-init replaced the KMS key file")
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes", "node4.pem")); err == nil {
		t.Error("a second dev-init wrote into a complete directory")
	}

	// A directory that is neither empty nor complete is not touched.
	partial := t.TempDir()
	writeTestFile(t, filepath.Join(partial, devKMSFile), "the key of some cluster")
	code, _, stderr = run("--dir", partial, "--nodes", "node1")
	if code != exitError || !strings.Contains(stderr, "not empty and not complete") {
		t.Errorf("dev-init into a partial directory: exit %d, %q", code, stderr)
	}
	if string(readTestFile(t, partial, devKMSFile)) != "the key of some cluster" {
		t.Fatal("dev-init overwrote a file it did not write")
	}

	empty := t.TempDir()
	usage := map[string][]string{
		"no directory":     {"--nodes", "a"},
		"no nodes":         {"--dir", empty},
		"stray argument":   {"--dir", empty, "--nodes", "a", "extra"},
		"zero validity":    {"--dir", empty, "--nodes", "a", "--validity", "0"},
		"unknown flag":     {"--dir", empty, "--nodes", "a", "--overwrite"},
		"both passphrases": {"--dir", empty, "--nodes", "a", "--kms-passphrase-file", "p"},
	}
	for name, args := range usage {
		if code, _, _ := run(args...); code != exitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, exitUsage)
		}
	}
	for name, args := range map[string][]string{
		"bad node ID": {"--dir", filepath.Join(empty, "a"), "--nodes", "Node_1"},
		"node twice":  {"--dir", filepath.Join(empty, "b"), "--nodes", "a,a"},
	} {
		if code, _, _ := run(args...); code != exitError {
			t.Errorf("%s: exit %d, want %d", name, code, exitError)
		}
	}
	if code, _, _ := runDevInitNoPassphrase(empty); code != exitUsage {
		t.Errorf("no passphrase: exit %d", code)
	}
	if code, stdout, _ := run("--help"); code != exitOK || stdout != "" {
		t.Errorf("--help: exit %d", code)
	}
	// Through the command line.
	var out, errOut bytes.Buffer
	if code := run2(t, &out, &errOut, env, "dev-init", "--dir", filepath.Join(empty, "c"), "--nodes", "a,b"); code != exitOK {
		t.Errorf("cactusfs dev-init: exit %d, %q", code, errOut.String())
	}
}

func runDevInitNoPassphrase(dir string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runDevInit([]string{"--dir", dir, "--nodes", "a"}, &stdout, &stderr, noEnv)
	return code, stdout.String(), stderr.String()
}

func run2(t *testing.T, stdout, stderr *bytes.Buffer, env func(string) string, args ...string) int {
	t.Helper()
	return run(t.Context(), args, stdout, stderr, env)
}
