package delivery_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// The marks stand for what must never be shown of a target: where it is, and
// anything of its two secrets, sealed as they are.
const (
	markEndpoint = "MARK-endpoint-5e1b"
	markSealed   = "MARK-sealed-77aa"
	markSigning  = "MARK-signkey-c0d3"
	// markPath stands for the path of a webhook, which may be a
	// credential: it is shown to nobody, whoever may read targets.
	markPath = "MARK-path-91c4"
)

func sealedMark(mark string) iam.SealedSecret {
	return iam.SealedSecret{KMSKeyID: mark + "-kms", WrappedKey: []byte(mark + "-wrapped"), Nonce: []byte(mark + "-nonce"), Ciphertext: []byte(mark + "-ciphertext")}
}

func webhook() delivery.Target {
	return delivery.Target{
		Name: "hook", Kind: delivery.KindWebhook, Endpoint: "https://" + markEndpoint + ".example.com", SealedPath: sealedMark(markPath),
		Buckets: []string{"photos", "logs-*"}, Auth: delivery.Auth{Mode: delivery.AuthBearer},
		Secret: sealedMark(markSealed), SignKey: sealedMark(markSigning),
		CreatedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC),
	}
}

func natsTarget() delivery.Target {
	return delivery.Target{
		Name: "bus", Kind: delivery.KindNATS, Endpoint: "tls://nats.example.com:4222", Subject: "cactusfs.events",
		Buckets: []string{"*"}, Audit: true, Auth: delivery.Auth{Mode: delivery.AuthBasic, ID: "cactus"}, Secret: sealedMark(markSealed),
	}
}

func site() delivery.Target {
	return delivery.Target{
		Name: "site-b", Kind: delivery.KindS3, Endpoint: "https://s3.site-b.example", Region: "eu-site-b",
		Buckets: []string{"photos"}, Auth: delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEB"}, Secret: sealedMark(markSealed),
	}
}

// certificate returns a certificate made for the test, in PEM, and the key it
// was made with, in PEM too.
func certificate(t testing.TB) (cert, key []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1<<31, 0), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
}

func TestTargetValidate(t *testing.T) {
	t.Parallel()
	cert, key := certificate(t)
	change := func(base delivery.Target, edit func(*delivery.Target)) delivery.Target {
		out := base.Clone()
		edit(&out)
		return out
	}
	valid := map[string]delivery.Target{
		"a webhook":                    webhook(),
		"a webhook that shows nothing": change(webhook(), func(x *delivery.Target) { x.Auth, x.Secret = delivery.Auth{}, iam.SealedSecret{} }),
		"a webhook of the mode none": change(webhook(), func(x *delivery.Target) {
			x.Auth, x.Secret = delivery.Auth{Mode: delivery.AuthNone}, iam.SealedSecret{}
		}),
		"a webhook without a signing key": change(webhook(), func(x *delivery.Target) { x.SignKey = iam.SealedSecret{} }),
		"a webhook without a path":        change(webhook(), func(x *delivery.Target) { x.SealedPath = iam.SealedSecret{} }),
		"a webhook on a port":             change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://hooks.example.com:8443" }),
		"a webhook at an address of IPv6": change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://[2001:db8::1]:8443" }),
		"a secret of a ciphertext alone": change(webhook(), func(x *delivery.Target) {
			x.Secret = iam.SealedSecret{Ciphertext: []byte{1}}
		}),
		"sealed fields at their bounds": change(webhook(), func(x *delivery.Target) {
			x.SealedPath = iam.SealedSecret{
				KMSKeyID: strings.Repeat("k", delivery.MaxSealedKeyBytes), WrappedKey: make([]byte, delivery.MaxSealedKeyBytes),
				Nonce: make([]byte, 64), Ciphertext: make([]byte, delivery.MaxSealedBytes),
			}
			x.Secret, x.SignKey = x.SealedPath, x.SealedPath
		}),
		"a webhook with a CA":             change(webhook(), func(x *delivery.Target) { x.CAPEM = bytes.Join([][]byte{cert, cert}, []byte("\n")) }),
		"a webhook that admits no bucket": change(webhook(), func(x *delivery.Target) { x.Buckets, x.Audit = nil, true }),
		"a NATS server":                   natsTarget(),
		"a NATS server with a token":      change(natsTarget(), func(x *delivery.Target) { x.Auth = delivery.Auth{Mode: delivery.AuthToken} }),
		"a NATS server that asks nothing": change(natsTarget(), func(x *delivery.Target) { x.Auth, x.Secret = delivery.Auth{}, iam.SealedSecret{} }),
		"another deployment":              site(),
		"a target that is referred to":    change(site(), func(x *delivery.Target) { x.Refs = 3 }),
	}
	for name, target := range valid {
		if err := target.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	many := make([]string, delivery.MaxTargetBuckets+1)
	for i := range many {
		many[i] = fmt.Sprintf("bucket-%d", i)
	}
	refused := map[string]delivery.Target{
		"no name":                             change(webhook(), func(x *delivery.Target) { x.Name = "" }),
		"a name in capitals":                  change(webhook(), func(x *delivery.Target) { x.Name = "Hook" }),
		"no kind":                             change(webhook(), func(x *delivery.Target) { x.Kind = "" }),
		"a kind there is not":                 change(webhook(), func(x *delivery.Target) { x.Kind = "kafka" }),
		"no endpoint":                         change(webhook(), func(x *delivery.Target) { x.Endpoint = "" }),
		"an endpoint in the clear":            change(webhook(), func(x *delivery.Target) { x.Endpoint = "http://" + markEndpoint + ".example.com" }),
		"an endpoint with its path":           change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://hooks.example.com/in/" + markPath }),
		"an endpoint that ends in a slash":    change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://hooks.example.com/" }),
		"an endpoint with a slash too many":   change(webhook(), func(x *delivery.Target) { x.Endpoint = "https:///" + markPath }),
		"an endpoint of a scheme and nothing": change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://" }),
		"an endpoint of a port and no host":   change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://:8443" }),
		"a NATS server of a port and no host": change(natsTarget(), func(x *delivery.Target) { x.Endpoint = "tls://:4222" }),
		"a secret of a key and no ciphertext": change(webhook(), func(x *delivery.Target) { x.Secret.Ciphertext = nil }),
		"a path of a nonce and no ciphertext": change(webhook(), func(x *delivery.Target) { x.SealedPath = iam.SealedSecret{Nonce: []byte{1}} }),
		"a signing key of a name and nothing": change(webhook(), func(x *delivery.Target) { x.SignKey = iam.SealedSecret{KMSKeyID: "k"} }),
		"a secret of the mode none, half there": change(webhook(), func(x *delivery.Target) {
			x.Auth, x.Secret = delivery.Auth{}, iam.SealedSecret{WrappedKey: []byte{1}}
		}),
		"a NATS server with a path":            change(natsTarget(), func(x *delivery.Target) { x.SealedPath = sealedMark(markPath) }),
		"another deployment with a path":       change(site(), func(x *delivery.Target) { x.SealedPath = sealedMark(markPath) }),
		"a sealed path that is too long":       change(webhook(), func(x *delivery.Target) { x.SealedPath.Ciphertext = make([]byte, delivery.MaxSealedBytes+1) }),
		"a secret that is too long":            change(webhook(), func(x *delivery.Target) { x.Secret.Ciphertext = make([]byte, delivery.MaxSealedBytes+1) }),
		"a signing key with a key too long":    change(webhook(), func(x *delivery.Target) { x.SignKey.WrappedKey = make([]byte, delivery.MaxSealedKeyBytes+1) }),
		"a sealed path with a name too long":   change(webhook(), func(x *delivery.Target) { x.SealedPath.KMSKeyID = strings.Repeat("k", delivery.MaxSealedKeyBytes+1) }),
		"a secret with a nonce too long":       change(webhook(), func(x *delivery.Target) { x.Secret.Nonce = make([]byte, 65) }),
		"an endpoint with a user":              change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://" + markEndpoint + "@hooks.example.com" }),
		"an endpoint with a query":             change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://hooks.example.com/in?key=" + markEndpoint }),
		"an endpoint with a fragment":          change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://hooks.example.com/in#" + markEndpoint }),
		"an endpoint with a space":             change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://hooks.example.com/ in" }),
		"an endpoint that is too long":         change(webhook(), func(x *delivery.Target) { x.Endpoint = "https://h/" + strings.Repeat("a", delivery.MaxEndpointLength) }),
		"a NATS server over HTTPS":             change(natsTarget(), func(x *delivery.Target) { x.Endpoint = "https://nats.example.com" }),
		"references below none":                change(webhook(), func(x *delivery.Target) { x.Refs = -1 }),
		"too many buckets":                     change(webhook(), func(x *delivery.Target) { x.Buckets = many }),
		"a bucket twice":                       change(webhook(), func(x *delivery.Target) { x.Buckets = []string{"a", "b", "a"} }),
		"an empty pattern":                     change(webhook(), func(x *delivery.Target) { x.Buckets = []string{""} }),
		"a pattern in capitals":                change(webhook(), func(x *delivery.Target) { x.Buckets = []string{"Photos"} }),
		"a pattern with a question mark":       change(webhook(), func(x *delivery.Target) { x.Buckets = []string{"photo?"} }),
		"a pattern that is too long":           change(webhook(), func(x *delivery.Target) { x.Buckets = []string{strings.Repeat("a", 64)} }),
		"a key among the certificates":         change(webhook(), func(x *delivery.Target) { x.CAPEM = append(bytes.Clone(cert), key...) }),
		"certificates that are no PEM":         change(webhook(), func(x *delivery.Target) { x.CAPEM = []byte("certificate") }),
		"something after the certificates":     change(webhook(), func(x *delivery.Target) { x.CAPEM = append(bytes.Clone(cert), "trailing"...) }),
		"too many certificates":                change(webhook(), func(x *delivery.Target) { x.CAPEM = bytes.Repeat(cert, delivery.MaxCAPEMBytes/len(cert)+1) }),
		"a user with a line break":             change(natsTarget(), func(x *delivery.Target) { x.Auth.ID = "cactus\nx" }),
		"a user that is too long":              change(natsTarget(), func(x *delivery.Target) { x.Auth.ID = strings.Repeat("u", 129) }),
		"a webhook with a region":              change(webhook(), func(x *delivery.Target) { x.Region = "eu" }),
		"a webhook with a subject":             change(webhook(), func(x *delivery.Target) { x.Subject = "s" }),
		"a webhook that signs as a deployment": change(webhook(), func(x *delivery.Target) { x.Auth = delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEB"} }),
		"a webhook with a token of NATS":       change(webhook(), func(x *delivery.Target) { x.Auth.Mode = delivery.AuthToken }),
		"a webhook with a user":                change(webhook(), func(x *delivery.Target) { x.Auth.ID = "cactus" }),
		"a bearer without a secret":            change(webhook(), func(x *delivery.Target) { x.Secret = iam.SealedSecret{} }),
		"a secret without a use":               change(webhook(), func(x *delivery.Target) { x.Auth = delivery.Auth{} }),
		"a mode there is not":                  change(webhook(), func(x *delivery.Target) { x.Auth.Mode = "mtls" }),
		"a NATS server without a subject":      change(natsTarget(), func(x *delivery.Target) { x.Subject = "" }),
		"a NATS subject with a wildcard":       change(natsTarget(), func(x *delivery.Target) { x.Subject = "cactusfs.*" }),
		"a NATS subject with a tail wildcard":  change(natsTarget(), func(x *delivery.Target) { x.Subject = "cactusfs.>" }),
		"a NATS subject with an empty token":   change(natsTarget(), func(x *delivery.Target) { x.Subject = "cactusfs..events" }),
		"a NATS subject with a space":          change(natsTarget(), func(x *delivery.Target) { x.Subject = "cactusfs events" }),
		"a NATS subject that is too long":      change(natsTarget(), func(x *delivery.Target) { x.Subject = strings.Repeat("s", 256) }),
		"a NATS server with a region":          change(natsTarget(), func(x *delivery.Target) { x.Region = "eu" }),
		"a NATS server with a bearer":          change(natsTarget(), func(x *delivery.Target) { x.Auth = delivery.Auth{Mode: delivery.AuthBearer} }),
		"a NATS user without a name":           change(natsTarget(), func(x *delivery.Target) { x.Auth.ID = "" }),
		"a NATS server with a signing key":     change(natsTarget(), func(x *delivery.Target) { x.SignKey = sealedMark(markSigning) }),
		"another deployment without a region":  change(site(), func(x *delivery.Target) { x.Region = "" }),
		"a region in capitals":                 change(site(), func(x *delivery.Target) { x.Region = "EU" }),
		"a region with a dot":                  change(site(), func(x *delivery.Target) { x.Region = "eu.west" }),
		"a region with a star":                 change(site(), func(x *delivery.Target) { x.Region = "eu-*" }),
		"a region that is too long":            change(site(), func(x *delivery.Target) { x.Region = strings.Repeat("r", 65) }),
		"another deployment with a subject":    change(site(), func(x *delivery.Target) { x.Subject = "s" }),
		"another deployment sent the trail":    change(site(), func(x *delivery.Target) { x.Audit = true }),
		"another deployment without a key":     change(site(), func(x *delivery.Target) { x.Auth, x.Secret = delivery.Auth{}, iam.SealedSecret{} }),
		"another deployment with a bearer":     change(site(), func(x *delivery.Target) { x.Auth = delivery.Auth{Mode: delivery.AuthBearer} }),
		"an access key that is none":           change(site(), func(x *delivery.Target) { x.Auth.ID = "A!" }),
		"an access key that is too short":      change(site(), func(x *delivery.Target) { x.Auth.ID = "AK" }),
		"another deployment with a signing key": change(site(), func(x *delivery.Target) {
			x.SignKey = sealedMark(markSigning)
		}),
	}
	// A sealed field is set exactly when it has a ciphertext, also in a
	// target that nobody validated: a key, a nonce or the name of a KMS key
	// by itself is no secret and no path, and is not shown as one.
	half := delivery.Target{
		Name: "hook", Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com",
		Secret: iam.SealedSecret{WrappedKey: []byte{1}}, SignKey: iam.SealedSecret{Nonce: []byte{1}}, SealedPath: iam.SealedSecret{KMSKeyID: "k"},
	}
	if shown := half.Shown(); half.SecretSet() || half.SignKeySet() || half.PathSet() || shown.SecretSet || shown.SignKeySet || shown.PathSet {
		t.Errorf("sealed fields without a ciphertext are told as set: %+v", shown)
	}
	half.Secret.Ciphertext, half.SignKey.Ciphertext, half.SealedPath.Ciphertext = []byte{1}, []byte{1}, []byte{1}
	if !half.SecretSet() || !half.SignKeySet() || !half.PathSet() {
		t.Error("sealed fields with a ciphertext are told as not set")
	}
	for name, target := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := target.Validate()
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("Validate = %v, want %v", err, domain.ErrInvalidArgument)
			}
			for _, mark := range []string{markEndpoint, markSealed, markSigning, markPath} {
				if strings.Contains(err.Error(), mark) {
					t.Errorf("the refusal tells what it must not: %v", err)
				}
			}
		})
	}
}

func TestCheckCA(t *testing.T) {
	t.Parallel()
	cert, key := certificate(t)
	if err := delivery.CheckCA(nil); err != nil {
		t.Errorf("no certificates: %v", err)
	}
	if err := delivery.CheckCA(append(bytes.Clone(cert), cert...)); err != nil {
		t.Errorf("two certificates: %v", err)
	}
	notOne := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})
	withHeader := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: []byte("x")})
	for name, data := range map[string][]byte{
		"a key": key, "a certificate and a key": append(bytes.Clone(cert), key...), "no PEM": []byte("hello"),
		"a block that is no certificate": notOne, "a block with headers": withHeader,
		"too much": bytes.Repeat(cert, delivery.MaxCAPEMBytes/len(cert)+1),
	} {
		if err := delivery.CheckCA(data); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: CheckCA = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	// The state machine asks for the framing only: what a certificate is
	// may differ from one build to the next, and members must not. Apply
	// judges bytes, the use case judges what they mean: each of these is
	// framed as a certificate, CheckCA refuses it, and Validate takes it.
	body := func(lines ...string) []byte {
		return []byte("-----BEGIN CERTIFICATE-----\n" + strings.Join(lines, "\n") + "\n-----END CERTIFICATE-----\n")
	}
	for name, data := range map[string][]byte{
		"bytes that are no certificate":         notOne,
		"a certificate cut short":               body(strings.Split(string(cert), "\n")[1]),
		"bits after the end of the last byte":   body("QR=="),
		"a certificate and one that is none":    append(bytes.Clone(cert), notOne...),
		"a certificate with a byte turned over": turned(t, cert),
	} {
		framed := webhook()
		framed.CAPEM = data
		if err := framed.Validate(); err != nil {
			t.Errorf("%s: Validate reads into the certificates: %v", name, err)
		}
		if err := delivery.CheckCA(data); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: CheckCA = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}

// turned returns the certificate with one character of its body changed
// for another of the alphabet: framed as before, and no certificate.
func turned(t *testing.T, cert []byte) []byte {
	t.Helper()
	lines := strings.Split(string(cert), "\n")
	if len(lines) < 4 || len(lines[2]) < 10 {
		t.Fatalf("a certificate of %d lines", len(lines))
	}
	replacement := "A"
	if lines[2][5] == 'A' {
		replacement = "B"
	}
	lines[2] = lines[2][:5] + replacement + lines[2][6:]
	return []byte(strings.Join(lines, "\n"))
}

// TestCAFraming is the rule of bytes the state machine refuses certificates
// by (Target.Validate), row by row.
func TestCAFraming(t *testing.T) {
	t.Parallel()
	cert, key := certificate(t)
	const begin, end = "-----BEGIN CERTIFICATE-----", "-----END CERTIFICATE-----"
	full := strings.Repeat("A", 64)
	tests := []struct {
		name  string
		data  string
		valid bool
	}{
		{"nothing", "", true},
		{"a certificate", string(cert), true},
		{"two certificates", string(cert) + string(cert), true},
		{"empty lines around and between", "\n\n" + string(cert) + "\n\n" + string(cert) + "\n", true},
		{"lines that end as on Windows", strings.ReplaceAll(string(cert), "\n", "\r\n"), true},
		{"no line break at the end", strings.TrimSuffix(string(cert), "\n"), true},
		{"only empty lines", "\n\r\n\n", true},
		{"one line of four", begin + "\nAAAA\n" + end, true},
		{"one sign of padding", begin + "\nAAA=\n" + end, true},
		{"two signs of padding", begin + "\nAA==\n" + end, true},
		{"a full line and a padded one", begin + "\n" + full + "\nAA==\n" + end, true},
		{"a short line before a full one", begin + "\nAAAA\n" + full + "\n" + end, true},
		{"every character of the alphabet", begin + "\nABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/\n" + end, true},

		{"a key", string(key), false},
		{"a certificate and a key", string(cert) + string(key), false},
		{"no PEM", "hello", false},
		{"text before the block", "my CA\n" + string(cert), false},
		{"text after the block", string(cert) + "my CA\n", false},
		{"a comment between blocks", string(cert) + "# next\n" + string(cert), false},
		{"a space line outside a block", " \n" + string(cert), false},
		{"another label", strings.ReplaceAll(string(cert), "CERTIFICATE", "TRUSTED CERTIFICATE"), false},
		{"a label in small letters", strings.ReplaceAll(string(cert), "BEGIN CERTIFICATE", "BEGIN certificate"), false},
		{"four dashes", strings.Replace(string(cert), "-----BEGIN", "----BEGIN", 1), false},
		{"a space after the first line", strings.Replace(string(cert), begin, begin+" ", 1), false},
		{"a tab before the first line", "\t" + string(cert), false},
		{"no end", begin + "\nAAAA\n", false},
		{"no beginning", "AAAA\n" + end + "\n", false},
		{"the end twice", begin + "\nAAAA\n" + end + "\n" + end, false},
		{"a block in a block", begin + "\nAAAA\n" + begin + "\nAAAA\n" + end + "\n" + end, false},
		{"an empty block", begin + "\n" + end, false},
		{"an empty line in the body", begin + "\nAAAA\n\nAAAA\n" + end, false},
		{"a header", begin + "\nProc-Type: 4,ENCRYPTED\n\nAAAA\n" + end, false},
		{"a line of sixty-five", begin + "\n" + full + "A\nAAA=\n" + end, false},
		{"a body that is no multiple of four", begin + "\nAAAAA\n" + end, false},
		{"three signs of padding", begin + "\nA===\n" + end, false},
		{"only padding", begin + "\n====\n" + end, false},
		{"padding in the middle of a line", begin + "\nAA=A\n" + end, false},
		{"a line after a padded one", begin + "\nAA==\nAAAA\n" + end, false},
		{"padding on a line of its own after padding", begin + "\nAAA=\n====\n" + end, false},
		{"the alphabet of URLs", begin + "\nAA-_\n" + end, false},
		{"a space in the body", begin + "\nAA A\n" + end, false},
		{"a space after the body", begin + "\nAAAA \n" + end, false},
		{"a carriage return alone", begin + "\rAAAA\r" + end, false},
		{"two carriage returns", begin + "\r\r\nAAAA\n" + end, false},
		{"a byte of nothing", begin + "\nAA\x00A\n" + end, false},
		{"a byte that is not ASCII", begin + "\nAAA\xc3\xa9\n" + end, false},
		{"too much", strings.Repeat(string(cert), delivery.MaxCAPEMBytes/len(cert)+1), false},
	}
	for _, tt := range tests {
		target := webhook()
		target.CAPEM = []byte(tt.data)
		err := target.Validate()
		if (err == nil) != tt.valid || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("%s: Validate = %v, valid wanted: %v", tt.name, err, tt.valid)
		}
		// What the framing refuses, CheckCA refuses.
		if !tt.valid && delivery.CheckCA([]byte(tt.data)) == nil {
			t.Errorf("%s: CheckCA takes what Validate refuses", tt.name)
		}
	}
	// As much as there can be is taken.
	padding := strings.Repeat("\n", delivery.MaxCAPEMBytes-len(cert))
	target := webhook()
	target.CAPEM = []byte(string(cert) + padding)
	if err := target.Validate(); err != nil {
		t.Errorf("certificates of exactly the bound: %v", err)
	}
	target.CAPEM = append(target.CAPEM, '\n')
	if err := target.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("certificates of one byte more: %v", err)
	}
}

// FuzzCAFraming holds the rule of bytes against the parser it must not call:
// whatever Validate takes as certificates is, to encoding/pem of this
// release, exactly blocks of certificates without headers and nothing else;
// and whatever CheckCA takes, Validate takes. Neither ever does anything but
// answer.
func FuzzCAFraming(f *testing.F) {
	cert, key := certificate(f)
	f.Add(cert)
	f.Add(key)
	f.Add(append(bytes.Clone(cert), cert...))
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----"))
	f.Add([]byte("-----BEGIN CERTIFICATE-----\r\nAAAA\r\n-----END CERTIFICATE-----\r\n\r\n"))
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nProc-Type: 4,ENCRYPTED\n\nAAAA\n-----END CERTIFICATE-----\n"))
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nAA=A\n-----END CERTIFICATE-----\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		target := webhook()
		target.CAPEM = data
		framed := target.Validate()
		if framed != nil && !errors.Is(framed, domain.ErrInvalidArgument) {
			t.Fatalf("Validate: %v", framed)
		}
		checked := delivery.CheckCA(data)
		if checked != nil && !errors.Is(checked, domain.ErrInvalidArgument) {
			t.Fatalf("CheckCA: %v", checked)
		}
		if checked == nil && framed != nil {
			t.Fatalf("CheckCA takes what Validate refuses: %v", framed)
		}
		if framed != nil {
			return
		}
		// The same answer for the same bytes, always.
		if again := target.Validate(); again != nil {
			t.Fatalf("Validate, asked again: %v", again)
		}
		rest, blocks := data, 0
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(block.Bytes) == 0 {
				t.Fatalf("framed as certificates, and read as a block of %q with %d headers and %d bytes", block.Type, len(block.Headers), len(block.Bytes))
			}
			blocks++
		}
		if strings.TrimSpace(string(rest)) != "" {
			t.Fatalf("framed as certificates, and %q is left over", rest)
		}
		if want := strings.Count(string(data), "-----BEGIN CERTIFICATE-----"); blocks != want {
			t.Fatalf("framed as %d certificates, read as %d", want, blocks)
		}
	})
}

// TestTargetsAreShownByNameAndKind: no way of printing a target - with a verb
// of fmt, in a log, through an error that wraps it - shows where it is or
// anything of its secrets.
func TestTargetsAreShownByNameAndKind(t *testing.T) {
	t.Parallel()
	target := webhook()
	var logged bytes.Buffer
	ctx := context.Background()
	slog.New(slog.NewJSONHandler(&logged, nil)).InfoContext(ctx, "target changed", slog.Any("target", target), slog.Any("pointer", &target))
	slog.New(slog.NewTextHandler(&logged, nil)).InfoContext(ctx, "target changed", slog.Any("target", target))
	shown := []string{
		target.String(),
		fmt.Sprint(target), fmt.Sprint(&target),
		fmt.Sprintf("%v|%+v|%#v|%s|%q|%x|%d", target, target, target, target, target, target, target),
		fmt.Sprintf("%v|%+v|%#v|%s", &target, &target, []delivery.Target{target}, map[string]delivery.Target{"t": target}),
		fmt.Errorf("targets.Put: %v: %w", target, domain.ErrConflict).Error(),
		logged.String(),
	}
	// As JSON too, whoever encodes it and however it is held.
	for _, held := range []any{target, &target, []delivery.Target{target}, map[string]delivery.Target{"t": target}, struct{ Target delivery.Target }{target}} {
		encoded, err := json.Marshal(held)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		shown = append(shown, string(encoded))
	}
	if encoded, err := json.Marshal(target); err != nil || string(encoded) != `{"name":"hook","kind":"webhook"}` {
		t.Errorf("a target as JSON = %s, %v", encoded, err)
	}
	// A name and a kind that are no name and no kind are still JSON.
	odd := delivery.Target{Name: "a\"b\n\xff<", Kind: "k\\"}
	var read struct{ Name, Kind string }
	if encoded, err := json.Marshal(odd); err != nil || json.Unmarshal(encoded, &read) != nil || read.Kind != "k\\" || !strings.HasPrefix(read.Name, "a\"b\n") {
		t.Errorf("an odd target as JSON = %s, %v, read as %+v", encoded, err, read)
	}
	for _, text := range shown {
		for _, mark := range []string{markEndpoint, markSealed, markSigning, markPath} {
			if strings.Contains(text, mark) {
				t.Errorf("a target is shown with %s: %s", mark, text)
			}
		}
	}
	if got := target.String(); got != "target hook (webhook)" {
		t.Errorf("String() = %q", got)
	}
	if !strings.Contains(logged.String(), `"name":"hook"`) || !strings.Contains(logged.String(), `"kind":"webhook"`) {
		t.Errorf("the log says too little: %s", logged.String())
	}

	// What may be shown has no secret at all, and still says where the
	// target is: that is for who may read targets.
	if !target.SecretSet() || !target.SignKeySet() {
		t.Error("the secrets of a target are not found")
	}
	open := target.Shown()
	if !open.SecretSet || !open.SignKeySet || open.Name != target.Name || open.Kind != target.Kind ||
		open.Origin != "https://"+markEndpoint+".example.com" || !open.PathSet || !open.CreatedAt.Equal(target.CreatedAt) || !open.UpdatedAt.Equal(target.UpdatedAt) {
		t.Errorf("Shown() = %+v", open)
	}
	// What may be shown says where the target is, and has neither a secret
	// nor the path, however it is printed or encoded.
	encoded, err := json.Marshal(open)
	if err != nil || !bytes.Contains(encoded, []byte(markEndpoint)) {
		t.Errorf("a target as it may be shown does not say where it is: %s, %v", encoded, err)
	}
	for _, text := range []string{string(encoded), fmt.Sprintf("%v|%+v|%#v", open, open, open)} {
		for _, mark := range []string{markSealed, markSigning, markPath} {
			if strings.Contains(text, mark) {
				t.Errorf("a target as it may be shown holds %s: %s", mark, text)
			}
		}
	}
	// It shares nothing with the target.
	open.Buckets[0] = "other"
	if target.Buckets[0] != "photos" {
		t.Error("what is shown of a target shares with it")
	}
	// A copy shares nothing with what it was made from.
	clone := target.Clone()
	clone.Buckets[0], clone.Secret.Ciphertext[0], clone.SignKey.Nonce[0] = "other", 'x', 'x'
	clone.SealedPath.Ciphertext[0], clone.SealedPath.WrappedKey[0], clone.SealedPath.Nonce[0] = 'x', 'x', 'x'
	if target.Buckets[0] != "photos" || target.Secret.Ciphertext[0] != 'M' || target.SignKey.Nonce[0] != 'M' ||
		target.SealedPath.Ciphertext[0] != 'M' || target.SealedPath.WrappedKey[0] != 'M' || target.SealedPath.Nonce[0] != 'M' {
		t.Error("a copy of a target shares with it")
	}
	// Each sealed field is sealed with additional data of its own, so that
	// one moved to another target or to another field does not open.
	if got := string(delivery.SecretAAD("hook")) + " " + string(delivery.SignKeyAAD("hook")) + " " + string(delivery.PathAAD("hook")); got != "target/hook/secret target/hook/sign_key target/hook/path" {
		t.Errorf("the additional data of the secrets: %q", got)
	}
}

func TestTargetAdmitsAndServes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		buckets []string
		bucket  string
		want    bool
	}{
		{nil, "photos", false},
		{[]string{}, "photos", false},
		{[]string{"photos"}, "photos", true},
		{[]string{"photos"}, "photos-2", false},
		{[]string{"photos"}, "photo", false},
		{[]string{"*"}, "photos", true},
		{[]string{"*"}, "", true},
		{[]string{"logs-*"}, "logs-2026", true},
		{[]string{"logs-*"}, "logs-", true},
		{[]string{"logs-*"}, "logs", false},
		{[]string{"*-logs"}, "eu-logs", true},
		{[]string{"*-logs"}, "eu-logs-old", false},
		{[]string{"a*b*c"}, "a-b-c", true},
		{[]string{"a*b*c"}, "abc", true},
		{[]string{"a*b*c"}, "a-c", false},
		{[]string{"a*b*c"}, "abcbcx", false},
		{[]string{"a**c"}, "abc", true},
		{[]string{"other", "logs-*", "photos"}, "photos", true},
		{[]string{"other", "logs-*"}, "photos", false},
		// A pattern has no other wildcard: a dot is a dot.
		{[]string{"a.b"}, "axb", false},
	}
	for _, tt := range tests {
		if got := (delivery.Target{Buckets: tt.buckets}).Admits(tt.bucket); got != tt.want {
			t.Errorf("a target of %v admits %q = %v, want %v", tt.buckets, tt.bucket, got, tt.want)
		}
	}
	serves := map[delivery.Kind][2]bool{
		delivery.KindWebhook: {true, false}, delivery.KindNATS: {true, false}, delivery.KindS3: {false, true}, "": {false, false},
	}
	for kind, want := range serves {
		if kind.Serves(delivery.ConsumerNotification) != want[0] || kind.Serves(delivery.ConsumerReplication) != want[1] || kind.Serves("x") {
			t.Errorf("what a %q target serves", kind)
		}
	}
	if delivery.Kind("kafka").Valid() || !delivery.KindS3.Valid() {
		t.Error("Kind.Valid")
	}
}

// FuzzTargetAdmits compares the patterns of a target with a slow matcher
// that is obviously right.
func FuzzTargetAdmits(f *testing.F) {
	f.Add("logs-*", "logs-2026")
	f.Add("a*b*c", "abcbc")
	f.Add("*", "")
	f.Add("**a", "ba")
	f.Fuzz(func(t *testing.T, pattern, name string) {
		if len(pattern) > 64 || len(name) > 64 {
			return
		}
		var slow func(p, s string) bool
		slow = func(p, s string) bool {
			if p == "" {
				return s == ""
			}
			if p[0] == '*' {
				for i := 0; i <= len(s); i++ {
					if slow(p[1:], s[i:]) {
						return true
					}
				}
				return false
			}
			return s != "" && p[0] == s[0] && slow(p[1:], s[1:])
		}
		if strings.Count(pattern, "*") > 6 {
			return
		}
		if got, want := (delivery.Target{Buckets: []string{pattern}}).Admits(name), slow(pattern, name); got != want {
			t.Fatalf("%q admits %q = %v, want %v", pattern, name, got, want)
		}
	})
}

// TestTargetOrigin: what is shown of where a target is, is everything before
// its path and never anything of it - cut by bytes, whatever the endpoint
// holds.
func TestTargetOrigin(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		endpoint, origin string
		pathSet          bool
	}{
		{"https://hooks.example.com", "https://hooks.example.com", false},
		{"https://hooks.example.com/", "https://hooks.example.com", false},
		{"https://hooks.example.com:8443", "https://hooks.example.com:8443", false},
		{"https://hooks.example.com:8443/", "https://hooks.example.com:8443", false},
		{"https://hooks.example.com/services/T000/B000/" + markPath, "https://hooks.example.com", true},
		{"https://hooks.example.com:8443/" + markPath, "https://hooks.example.com:8443", true},
		{"https://hooks.example.com//", "https://hooks.example.com", true},
		{"https://[2001:db8::1]:8443/" + markPath, "https://[2001:db8::1]:8443", true},
		{"https://10.1.2.3/in/" + markPath + "/", "https://10.1.2.3", true},
		{"tls://nats.example.com:4222", "tls://nats.example.com:4222", false},
		// What no use case proposed is cut the same way: nothing after
		// the first slash of the host, and nothing at all without "://".
		{"https:///" + markPath, "https://", true},
		{"https://host/a://" + markPath, "https://host", true},
		{markPath, "", true},
		{"https:/" + markPath, "", true},
		{"", "", false},
	} {
		origin, path := delivery.SplitEndpoint(tt.endpoint)
		if pathSet := path != "" && path != "/"; origin != tt.origin || pathSet != tt.pathSet || strings.Contains(origin, markPath) || origin+path != tt.endpoint {
			t.Errorf("SplitEndpoint(%q) = %q, %q; want the origin %q and a path: %v", tt.endpoint, origin, path, tt.origin, tt.pathSet)
		}
		// A target that nobody validated shows no more than that, and says
		// it has a path exactly when a sealed one is kept with it.
		target := delivery.Target{Name: "hook", Kind: delivery.KindWebhook, Endpoint: tt.endpoint}
		if got := target.Origin(); got != tt.origin || target.PathSet() || strings.Contains(got, markPath) {
			t.Errorf("%q: Origin() = %q, PathSet() = %v; want %q and no path", tt.endpoint, got, target.PathSet(), tt.origin)
		}
		target.SealedPath = sealedMark(markPath)
		if shown := target.Shown(); shown.Origin != tt.origin || !shown.PathSet {
			t.Errorf("%q: Shown() = %+v", tt.endpoint, shown)
		}
		// What the cut gives is what a target keeps: an endpoint Validate
		// takes exactly when it is an origin of the scheme that names a
		// host.
		kept := delivery.Target{Name: "hook", Kind: delivery.KindWebhook, Endpoint: origin}
		if err := kept.Validate(); (err == nil) != (strings.HasPrefix(origin, "https://") && origin != "https://") {
			t.Errorf("a webhook at the origin %q: Validate = %v", origin, err)
		}
	}
}

// FuzzTargetOrigin: nothing after the first slash of the host is ever part
// of what is shown, and for an endpoint CheckEndpoint takes, what is shown
// is the scheme, the host and the port it found.
func FuzzTargetOrigin(f *testing.F) {
	f.Add("https://hooks.example.com/services/T000/B000/XXXX")
	f.Add("https://[2001:db8::1]:8443/in")
	f.Add("tls://nats.example.com:4222")
	f.Add("https:///x")
	f.Add("no scheme")
	f.Fuzz(func(t *testing.T, endpoint string) {
		origin, path := delivery.SplitEndpoint(endpoint)
		target := delivery.Target{Name: "hook", Kind: delivery.KindWebhook, Endpoint: endpoint}
		if target.Origin() != origin || origin+path != endpoint || target.PathSet() {
			t.Fatalf("SplitEndpoint(%q) = %q, %q; Origin() = %q, PathSet() = %v", endpoint, origin, path, target.Origin(), target.PathSet())
		}
		pathSet := path != "" && path != "/"
		if !strings.HasPrefix(endpoint, origin) {
			t.Fatalf("Origin(%q) = %q is not its beginning", endpoint, origin)
		}
		if origin != "" && strings.Contains(origin[strings.Index(origin, "://")+3:], "/") {
			t.Fatalf("Origin(%q) = %q holds a path", endpoint, origin)
		}
		rest := endpoint[len(origin):]
		if origin != "" && rest != "" && rest[0] != '/' {
			t.Fatalf("Origin(%q) = %q ends before the host does", endpoint, origin)
		}
		if rest != path {
			t.Fatalf("SplitEndpoint(%q): the path %q, the rest %q", endpoint, path, rest)
		}
		parsed, err := delivery.CheckEndpoint(delivery.KindWebhook, endpoint)
		if err != nil {
			return
		}
		// The parser may write the path another way than it was typed;
		// where it begins, and whether there is one, it says as the cut.
		if (parsed.Path == "") != (rest == "") {
			t.Fatalf("CheckEndpoint(%q) finds the path %q, the cut %q", endpoint, parsed.Path, rest)
		}
		if parsed.PathSet() != pathSet {
			t.Fatalf("%q: a path by CheckEndpoint %v, by the cut %v", endpoint, parsed.PathSet(), pathSet)
		}
		// What the use case keeps of an endpoint it took - its origin, and
		// the path sealed - is a target the state machine keeps.
		kept := delivery.Target{Name: "hook", Kind: delivery.KindWebhook, Endpoint: origin}
		if pathSet {
			kept.SealedPath = sealedMark(markPath)
		}
		if err := kept.Validate(); err != nil {
			t.Fatalf("a webhook at the origin of %q: %v", endpoint, err)
		}
	})
}
