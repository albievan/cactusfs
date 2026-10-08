// Package vault is the KMS adapter for HashiCorp Vault's Transit secrets
// engine. It implements ports.KMS with Vault's HTTP API and the standard
// library; no Vault SDK is linked.
//
// The KMS key never leaves Vault. A data key is made here, from the node's
// own secure random source, and wrapped by Transit's encrypt operation with
// the caller's associated data bound into the ciphertext; unwrapping is
// Transit's decrypt with the same associated data. The Transit key must be
// of an AEAD type (aes256-gcm96 or chacha20-poly1305), which is what makes
// that binding possible.
//
// The token is read from a file every time it is used, so that an agent can
// renew or replace it without a restart. It is never logged.
package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	dataKeySize    = 32
	maxTokenFile   = 16 << 10
	maxResponse    = 1 << 20
	requestTimeout = 15 * time.Second
	defaultMount   = "transit"
)

// Config configures the adapter.
type Config struct {
	// Address is the URL of Vault, https://host:port. Plain http is
	// accepted only for a loopback IP address and only with
	// AllowPlaintextLoopback: key material crosses this connection.
	Address string
	// Mount is the path of the Transit engine; "transit" when empty.
	Mount string
	// Key is the name of the Transit key.
	Key string
	// TokenFile is the path of a file that holds the Vault token.
	TokenFile string
	// Namespace is the Vault Enterprise namespace, if any.
	Namespace string
	// CACertFile, when set, is the PEM file of the CA that issued Vault's
	// certificate; the system's CAs are used otherwise.
	CACertFile string
	// ServerName, when set, is the name expected in Vault's certificate,
	// for when Vault is reached under another name.
	ServerName string
	// AllowPlaintextLoopback permits http:// to a loopback IP address, for
	// development.
	AllowPlaintextLoopback bool
	// Entropy is the source of data keys. It must be cryptographically
	// secure.
	Entropy io.Reader
	// Dial, when set, replaces the dialer. Tests use it.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// KMS wraps and unwraps data keys with a Transit key.
type KMS struct {
	base      *url.URL
	mount     string
	key       string
	tokenFile string
	namespace string
	entropy   io.Reader
	client    *http.Client
}

var _ ports.KMS = (*KMS)(nil)

// Open returns the adapter and proves that it works: a probe is wrapped and
// unwrapped, so that a wrong address, token, key name or key type stops the
// node at start instead of at the first object.
func Open(ctx context.Context, cfg Config) (*KMS, error) {
	k, err := newKMS(cfg)
	if err != nil {
		return nil, fmt.Errorf("kms/vault.Open: %w", err)
	}
	probe := []byte("cactusfs-kms-probe")
	key, err := k.GenerateDataKey(ctx, probe)
	if err != nil {
		return nil, fmt.Errorf("kms/vault.Open: %w", err)
	}
	defer clear(key.Plaintext)
	back, err := k.UnwrapDataKey(ctx, key.Wrapped, probe)
	if err != nil {
		return nil, fmt.Errorf("kms/vault.Open: %w", err)
	}
	defer clear(back)
	if !bytes.Equal(back, key.Plaintext) {
		return nil, fmt.Errorf("kms/vault.Open: %w: Vault returned another key than it was given", domain.ErrKeyUnavailable)
	}
	// The associated data must really be bound: a key type without AEAD
	// would ignore it, and a wrapped key could then be moved to another
	// object.
	moved, err := k.UnwrapDataKey(ctx, key.Wrapped, []byte("another owner"))
	if err == nil {
		clear(moved)
		return nil, fmt.Errorf("kms/vault.Open: %w: the Transit key %s does not bind associated data; "+
			"use an AEAD key type such as aes256-gcm96", domain.ErrInvalidArgument, cfg.Key)
	}
	// Only Vault saying no is proof. An error on the way there proves
	// nothing about the key.
	if !errors.Is(err, errRefused) {
		return nil, fmt.Errorf("kms/vault.Open: %w", err)
	}
	return k, nil
}

func newKMS(cfg Config) (*KMS, error) {
	base, err := url.Parse(cfg.Address)
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("%w: Vault address %q: want https://host:port", domain.ErrInvalidArgument, cfg.Address)
	}
	switch base.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(base.Hostname())
		if !cfg.AllowPlaintextLoopback || ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("%w: Vault must be reached over https: keys cross this connection "+
				"(http is accepted for a loopback IP address in development only)", domain.ErrInvalidArgument)
		}
	default:
		return nil, fmt.Errorf("%w: Vault address %q: want https://host:port", domain.ErrInvalidArgument, cfg.Address)
	}
	if cfg.Key == "" || strings.ContainsAny(cfg.Key, "/?#% ") {
		return nil, fmt.Errorf("%w: Transit key name %q", domain.ErrInvalidArgument, cfg.Key)
	}
	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("%w: a Vault token file is required", domain.ErrInvalidArgument)
	}
	mount := strings.Trim(cfg.Mount, "/")
	if mount == "" {
		mount = defaultMount
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName}
	if cfg.CACertFile != "" {
		pem, err := readFile(cfg.CACertFile, maxResponse)
		if err != nil {
			return nil, fmt.Errorf("the CA certificate of Vault: %w", err)
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: %s holds no certificate", domain.ErrInvalidArgument, cfg.CACertFile)
		}
	}
	dial := cfg.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &KMS{
		base: base, mount: mount, key: cfg.Key, tokenFile: cfg.TokenFile, namespace: cfg.Namespace, entropy: cfg.Entropy,
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				DialContext: dial, TLSClientConfig: tlsConfig, MaxIdleConnsPerHost: 8, IdleConnTimeout: time.Minute,
				// Never follow Vault's address through a proxy from the
				// environment: the token would go with it.
				Proxy: nil,
			},
			// A redirect would carry the token to wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func readFile(path string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s is too large", domain.ErrInvalidArgument, path)
	}
	return data, nil
}

// errRefused marks an answer in which Vault turned a request down.
var errRefused = errors.New("refused by Vault")

// maxErrorText bounds how much of what Vault says goes into an error.
const maxErrorText = 256

// KeyID implements ports.KMS. It names the Transit key, not the server: the
// same key reached at another address is the same key. A namespace is part
// of the name: the same path in another namespace is another key.
func (k *KMS) KeyID() string {
	if k.namespace != "" {
		return "vault:" + k.namespace + ":" + k.mount + "/" + k.key
	}
	return "vault:" + k.mount + "/" + k.key
}

// GenerateDataKey implements ports.KMS.
func (k *KMS) GenerateDataKey(ctx context.Context, aad []byte) (ports.DataKey, error) {
	plaintext := make([]byte, dataKeySize)
	if _, err := io.ReadFull(k.entropy, plaintext); err != nil {
		return ports.DataKey{}, fmt.Errorf("kms/vault.GenerateDataKey: %w", err)
	}
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	err := k.call(ctx, "encrypt", map[string]string{
		"plaintext":       base64.StdEncoding.EncodeToString(plaintext),
		"associated_data": base64.StdEncoding.EncodeToString(aad),
	}, &out)
	if err == nil && out.Data.Ciphertext == "" {
		err = fmt.Errorf("%w: Vault returned no ciphertext", domain.ErrKeyUnavailable)
	}
	if err != nil {
		clear(plaintext)
		return ports.DataKey{}, fmt.Errorf("kms/vault.GenerateDataKey: %w", err)
	}
	return ports.DataKey{Plaintext: plaintext, Wrapped: []byte(out.Data.Ciphertext)}, nil
}

// UnwrapDataKey implements ports.KMS.
func (k *KMS) UnwrapDataKey(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	err := k.call(ctx, "decrypt", map[string]string{
		"ciphertext":      string(wrapped),
		"associated_data": base64.StdEncoding.EncodeToString(aad),
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("kms/vault.UnwrapDataKey: %w", err)
	}
	plaintext, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil || len(plaintext) != dataKeySize {
		clear(plaintext)
		return nil, fmt.Errorf("kms/vault.UnwrapDataKey: %w: Vault returned something that is not a data key", domain.ErrKeyUnavailable)
	}
	return plaintext, nil
}

// call performs one Transit operation on the key.
func (k *KMS) call(ctx context.Context, operation string, in map[string]string, out any) error {
	token, err := readFile(k.tokenFile, maxTokenFile)
	if err != nil {
		return fmt.Errorf("%w: Vault token file: %w", domain.ErrKeyUnavailable, err)
	}
	defer clear(token)
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	defer clear(body)
	endpoint := k.base.JoinPath("v1", k.mount, operation, k.key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", string(bytes.TrimSpace(token)))
	req.Header.Set("Content-Type", "application/json")
	if k.namespace != "" {
		req.Header.Set("X-Vault-Namespace", k.namespace)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The error of the HTTP client names the URL, never a header.
		return fmt.Errorf("%w: Vault cannot be reached: %w", domain.ErrKeyUnavailable, redact(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("%w: reading Vault's answer: %w", domain.ErrKeyUnavailable, err)
	}
	defer clear(data)
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(data, &failure)
		said := strings.Join(failure.Errors, "; ")
		if len(said) > maxErrorText {
			said = said[:maxErrorText]
		}
		refusal := fmt.Errorf("%w: Vault refused to %s with key %s: status %d: %s",
			domain.ErrKeyUnavailable, operation, k.key, resp.StatusCode, said)
		if resp.StatusCode == http.StatusBadRequest {
			// Vault looked at the request and turned it down, as opposed
			// to being sealed, unreachable or unwilling to talk to us.
			return fmt.Errorf("%w: %w", errRefused, refusal)
		}
		return refusal
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: Vault's answer is not JSON", domain.ErrKeyUnavailable)
	}
	return nil
}

// redact strips an error of the request it was made for.
func redact(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
