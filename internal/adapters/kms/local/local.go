// Package local is the KMS adapter for standalone and development use: a
// master key kept in a local file, sealed under a key derived from a
// passphrase with Argon2id. It must be chosen explicitly; there is no mode in
// which the master key or a data key is stored unencrypted.
//
// Clusters in production use the Vault Transit adapter instead.
package local

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"

	"github.com/albievan/cactusfs/internal/adapters/fsutil"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	fileMagic     = "cactusfs-kms-local"
	formatVersion = 1
	algorithm     = "argon2id"
	keySize       = 32
	saltSize      = 16
	keyIDSize     = 8
	keyIDPrefix   = "local:"
	fileMode      = 0o600

	// wrapVersion1 is the first format of a wrapped key: every data key
	// sealed directly under the master key with a random nonce. It is
	// always read, and written only until every node of the cluster reads
	// version 2 (ADR-0031).
	wrapVersion1 = 1
	// wrapVersion2 seals every data key under a key of its own, derived
	// from the master key and a random salt carried in the wrapped key.
	wrapVersion2 = 2
	// wrapSaltSize is the size of the salt of a version 2 wrapped key.
	wrapSaltSize = 32
	// wrapInfo is the HKDF context of the key a version 2 wrapped key is
	// sealed under; the key ID follows it.
	wrapInfo = "cactusfs kms/local wrap v2 "

	// MinPassphraseLength is the shortest passphrase accepted.
	MinPassphraseLength = 12
)

// Params are the Argon2id cost parameters of a key file.
type Params struct {
	// Time is the number of passes.
	Time uint32 `json:"time"`
	// MemoryKiB is the memory used, in KiB.
	MemoryKiB uint32 `json:"memory_kib"`
	// Threads is the degree of parallelism.
	Threads uint8 `json:"threads"`
}

// DefaultParams returns the second recommended parameter set of RFC 9106:
// 3 passes over 64 MiB with 4 lanes.
func DefaultParams() Params {
	return Params{Time: 3, MemoryKiB: 64 << 10, Threads: 4}
}

// Bounds on the parameters accepted from a key file. The lower bounds refuse
// a file weakened to make a stolen copy cheap to attack; the upper bounds
// refuse a file crafted to exhaust the machine.
func (p Params) validate() error {
	if p.Time < 1 || p.Time > 16 || p.MemoryKiB < 8<<10 || p.MemoryKiB > 4<<20 || p.Threads < 1 || p.Threads > 64 {
		return fmt.Errorf("%w: Argon2id parameters out of range", domain.ErrInvalidArgument)
	}
	return nil
}

// keyFile is the JSON content of a key file.
type keyFile struct {
	Magic         string `json:"magic"`
	FormatVersion int    `json:"format_version"`
	KeyID         string `json:"key_id"`
	KDF           struct {
		Algorithm string `json:"algorithm"`
		Params
		Salt []byte `json:"salt"`
	} `json:"kdf"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// KMS is a ports.KMS backed by a sealed local key file.
type KMS struct {
	keyID string
	// master is the master key; it never leaves the process.
	master []byte
	// aead is AES-256-GCM under the master key itself, which only
	// version 1 wrapped keys use.
	aead cipher.AEAD
	// version2Ready, when set, says whether every node that may read a
	// wrapped key reads version 2; until it does, keys are wrapped in
	// version 1. version2 records that it once did: no node that reads only
	// version 1 can join again after that.
	version2Ready atomic.Pointer[func() bool]
	version2      atomic.Bool
}

// WriteVersion2When makes the KMS wrap new data keys in version 1 until
// ready reports true, and in version 2 from then on, whatever ready reports
// later (ADR-0031). The composition root of a cluster sets it from the
// metadata group's level, so that a node of the release before version 2,
// sharing the key file during a rolling upgrade, can open every key wrapped
// while it is still a member. Without it, every key is wrapped in version 2.
// ready is asked for every new data key until it reports true, and must be
// quick and safe to call from any goroutine. Keys of both versions are
// always read.
func (k *KMS) WriteVersion2When(ready func() bool) {
	k.version2.Store(false)
	k.version2Ready.Store(&ready)
}

// writesVersion2 reports whether a new data key is wrapped in version 2.
func (k *KMS) writesVersion2() bool {
	if k.version2.Load() {
		return true
	}
	ready := k.version2Ready.Load()
	if ready == nil || (*ready)() {
		k.version2.Store(true)
		return true
	}
	return false
}

var _ ports.KMS = (*KMS)(nil)

// Create generates a new master key and writes it, sealed under passphrase,
// to a new key file at path. It refuses to overwrite an existing file.
// entropy must be a cryptographically secure source.
func Create(path string, passphrase []byte, params Params, entropy io.Reader) error {
	if len(passphrase) < MinPassphraseLength {
		return fmt.Errorf("kms/local.Create: %w: passphrase must be at least %d bytes",
			domain.ErrInvalidArgument, MinPassphraseLength)
	}
	if err := params.validate(); err != nil {
		return fmt.Errorf("kms/local.Create: %w", err)
	}

	random := make([]byte, keySize+saltSize+keyIDSize)
	if _, err := io.ReadFull(entropy, random); err != nil {
		return fmt.Errorf("kms/local.Create: read randomness: %w", err)
	}
	master, salt, id := random[:keySize], random[keySize:keySize+saltSize], random[keySize+saltSize:]
	defer clear(master)

	var file keyFile
	file.Magic = fileMagic
	file.FormatVersion = formatVersion
	file.KeyID = keyIDPrefix + hex.EncodeToString(id)
	file.KDF.Algorithm = algorithm
	file.KDF.Params = params
	file.KDF.Salt = salt

	sealer, err := sealingAEAD(passphrase, file)
	if err != nil {
		return fmt.Errorf("kms/local.Create: %w", err)
	}
	file.Nonce = make([]byte, sealer.NonceSize())
	if _, err := io.ReadFull(entropy, file.Nonce); err != nil {
		return fmt.Errorf("kms/local.Create: read randomness: %w", err)
	}
	file.Ciphertext = sealer.Seal(nil, file.Nonce, master, []byte(file.KeyID))

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("kms/local.Create: %w", err)
	}
	if err := writeNew(path, append(data, '\n')); err != nil {
		return fmt.Errorf("kms/local.Create: %w", err)
	}
	return nil
}

// Open unseals the key file at path with passphrase. The error wraps
// domain.ErrKeyUnavailable when the passphrase is wrong or the file has been
// modified.
func Open(path string, passphrase []byte) (*KMS, error) {
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("kms/local.Open: %w", err)
	}
	defer func() { _ = dir.Close() }()
	data, err := dir.ReadFile(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("kms/local.Open: %w", err)
	}

	var file keyFile
	if err := json.Unmarshal(data, &file); err != nil || file.Magic != fileMagic {
		return nil, fmt.Errorf("kms/local.Open: %w: %s is not a key file", domain.ErrKeyUnavailable, path)
	}
	if file.FormatVersion != formatVersion || file.KDF.Algorithm != algorithm {
		return nil, fmt.Errorf("kms/local.Open: %w: key file version %d (%s)",
			domain.ErrUnsupportedFormat, file.FormatVersion, file.KDF.Algorithm)
	}
	if !validKeyID(file.KeyID) {
		return nil, fmt.Errorf("kms/local.Open: %w: malformed key ID", domain.ErrKeyUnavailable)
	}
	if err := file.KDF.validate(); err != nil || len(file.KDF.Salt) != saltSize {
		return nil, fmt.Errorf("kms/local.Open: %w: key file parameters are out of range", domain.ErrKeyUnavailable)
	}

	sealer, err := sealingAEAD(passphrase, file)
	if err != nil {
		return nil, fmt.Errorf("kms/local.Open: %w", err)
	}
	if len(file.Nonce) != sealer.NonceSize() {
		return nil, fmt.Errorf("kms/local.Open: %w: malformed key file", domain.ErrKeyUnavailable)
	}
	master, err := sealer.Open(nil, file.Nonce, file.Ciphertext, []byte(file.KeyID))
	if err != nil || len(master) != keySize {
		return nil, fmt.Errorf("kms/local.Open: %w: wrong passphrase or modified key file", domain.ErrKeyUnavailable)
	}
	aead, err := newGCM(master)
	if err != nil {
		clear(master)
		return nil, fmt.Errorf("kms/local.Open: %w", err)
	}
	return &KMS{keyID: file.KeyID, master: master, aead: aead}, nil
}

// validKeyID reports whether id has the fixed form "local:" plus 16
// hexadecimal characters. The fixed length is what makes the associated data
// of a wrapped key, key ID followed by owner, unambiguous.
func validKeyID(id string) bool {
	suffix, ok := strings.CutPrefix(id, keyIDPrefix)
	if !ok || len(suffix) != hex.EncodedLen(keyIDSize) {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

// Exists reports whether a key file is present at path.
func Exists(path string) (bool, error) {
	dir, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kms/local.Exists: %w", err)
	}
	defer func() { _ = dir.Close() }()
	switch _, err := dir.Lstat(filepath.Base(path)); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("kms/local.Exists: %w", err)
	}
}

// sealingAEAD derives the key-encryption key from the passphrase and returns
// the AEAD that seals the master key. The derivation parameters are part of
// the file, so they are authenticated implicitly: changing them changes the
// derived key.
func sealingAEAD(passphrase []byte, file keyFile) (cipher.AEAD, error) {
	kek := argon2.IDKey(passphrase, file.KDF.Salt, file.KDF.Time, file.KDF.MemoryKiB, file.KDF.Threads, keySize)
	defer clear(kek)
	return newGCM(kek)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// writeNew creates path, which must not exist, with data and its directory
// entry flushed to stable storage: everything stored afterwards depends on
// this file surviving a power loss.
func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return fsutil.WriteNewDurable(dir, filepath.Base(path), data, fileMode)
}

// KeyID implements ports.KMS.
func (k *KMS) KeyID() string {
	return k.keyID
}

// GenerateDataKey implements ports.KMS. A wrapped key is, in version 2
// (ADR-0031),
//
//	version (1 byte, 2) || salt (32 bytes) || nonce (12 bytes) ||
//	AES-256-GCM(HKDF-SHA256(master key, salt, info || key ID), data key)
//
// with the key ID and the caller's aad as associated data. Every data key is
// sealed under a key of its own, so the number of keys wrapped under one
// master key is not bounded by the 2^32 random nonces AES-GCM allows a key.
// Until WriteVersion2When's ready says otherwise, a key is wrapped in
// version 1:
//
//	version (1 byte, 1) || nonce (12 bytes) || AES-256-GCM(master key, data key)
func (k *KMS) GenerateDataKey(ctx context.Context, aad []byte) (ports.DataKey, error) {
	if err := ctx.Err(); err != nil {
		return ports.DataKey{}, fmt.Errorf("kms/local.GenerateDataKey: %w", err)
	}
	nonceSize := k.aead.NonceSize()
	if !k.writesVersion2() {
		random := make([]byte, keySize+nonceSize)
		if _, err := io.ReadFull(randomSource(), random); err != nil {
			return ports.DataKey{}, fmt.Errorf("kms/local.GenerateDataKey: read randomness: %w", err)
		}
		plaintext, nonce := random[:keySize], random[keySize:]
		wrapped := make([]byte, 0, 1+nonceSize+keySize+k.aead.Overhead())
		wrapped = append(wrapped, wrapVersion1)
		wrapped = append(wrapped, nonce...)
		wrapped = k.aead.Seal(wrapped, nonce, plaintext, k.wrapAAD(aad))
		return ports.DataKey{Plaintext: plaintext, Wrapped: wrapped}, nil
	}
	// One read of the system source for the key, the salt and the nonce.
	random := make([]byte, keySize+wrapSaltSize+nonceSize)
	if _, err := io.ReadFull(randomSource(), random); err != nil {
		return ports.DataKey{}, fmt.Errorf("kms/local.GenerateDataKey: read randomness: %w", err)
	}
	plaintext, salt, nonce := random[:keySize], random[keySize:keySize+wrapSaltSize], random[keySize+wrapSaltSize:]

	sealer, err := k.wrappingAEAD(salt)
	if err != nil {
		return ports.DataKey{}, fmt.Errorf("kms/local.GenerateDataKey: %w", err)
	}
	wrapped := make([]byte, 0, 1+wrapSaltSize+nonceSize+keySize+sealer.Overhead())
	wrapped = append(wrapped, wrapVersion2)
	wrapped = append(wrapped, salt...)
	wrapped = append(wrapped, nonce...)
	wrapped = sealer.Seal(wrapped, nonce, plaintext, k.wrapAAD(aad))
	return ports.DataKey{Plaintext: plaintext, Wrapped: wrapped}, nil
}

// UnwrapDataKey implements ports.KMS. It reads both versions of a wrapped
// key.
func (k *KMS) UnwrapDataKey(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kms/local.UnwrapDataKey: %w", err)
	}
	malformed := fmt.Errorf("kms/local.UnwrapDataKey: %w: malformed wrapped key", domain.ErrKeyUnavailable)
	if len(wrapped) == 0 {
		return nil, malformed
	}
	nonceSize := k.aead.NonceSize()
	var sealer cipher.AEAD
	var rest []byte
	switch wrapped[0] {
	case wrapVersion1:
		sealer, rest = k.aead, wrapped[1:]
	case wrapVersion2:
		if len(wrapped) < 1+wrapSaltSize {
			return nil, malformed
		}
		var err error
		if sealer, err = k.wrappingAEAD(wrapped[1 : 1+wrapSaltSize]); err != nil {
			return nil, fmt.Errorf("kms/local.UnwrapDataKey: %w", err)
		}
		rest = wrapped[1+wrapSaltSize:]
	default:
		return nil, malformed
	}
	if len(rest) < nonceSize {
		return nil, malformed
	}
	nonce, ciphertext := rest[:nonceSize], rest[nonceSize:]
	plaintext, err := sealer.Open(nil, nonce, ciphertext, k.wrapAAD(aad))
	if err != nil || len(plaintext) != keySize {
		return nil, fmt.Errorf("kms/local.UnwrapDataKey: %w: key was not wrapped by %s for this owner",
			domain.ErrKeyUnavailable, k.keyID)
	}
	return plaintext, nil
}

// wrappingAEAD returns AES-256-GCM under the key one version 2 wrapped key
// is sealed under: HKDF-SHA256 of the master key with the wrapped key's salt,
// bound to this KMS key by the context.
func (k *KMS) wrappingAEAD(salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, k.master, salt, wrapInfo+k.keyID, keySize)
	if err != nil {
		return nil, fmt.Errorf("derive wrapping key: %w", err)
	}
	defer clear(key)
	return newGCM(key)
}

// wrapAAD binds a wrapped key to this KMS key and to its owner. The key ID
// has a fixed length, so the concatenation is unambiguous.
func (k *KMS) wrapAAD(aad []byte) []byte {
	out := make([]byte, 0, len(k.keyID)+len(aad))
	out = append(out, k.keyID...)
	return append(out, aad...)
}
