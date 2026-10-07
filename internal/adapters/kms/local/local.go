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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"

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
	wrapVersion   = 1
	fileMode      = 0o600

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
	aead  cipher.AEAD
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
	file.KeyID = "local:" + hex.EncodeToString(id)
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
	defer clear(master)

	aead, err := newGCM(master)
	if err != nil {
		return nil, fmt.Errorf("kms/local.Open: %w", err)
	}
	return &KMS{keyID: file.KeyID, aead: aead}, nil
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

// writeNew creates path, which must not exist, with data flushed to stable
// storage.
func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()

	f, err := dir.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// KeyID implements ports.KMS.
func (k *KMS) KeyID() string {
	return k.keyID
}

// GenerateDataKey implements ports.KMS. A wrapped key is
//
//	version (1 byte) || nonce (12 bytes) || AES-256-GCM(master key, data key)
//
// with the key ID and the caller's aad as associated data.
func (k *KMS) GenerateDataKey(ctx context.Context, aad []byte) (ports.DataKey, error) {
	if err := ctx.Err(); err != nil {
		return ports.DataKey{}, fmt.Errorf("kms/local.GenerateDataKey: %w", err)
	}
	// One read of the system source for the key and the nonce.
	random := make([]byte, keySize+k.aead.NonceSize())
	if _, err := io.ReadFull(randomSource(), random); err != nil {
		return ports.DataKey{}, fmt.Errorf("kms/local.GenerateDataKey: read randomness: %w", err)
	}
	plaintext, nonce := random[:keySize], random[keySize:]

	wrapped := make([]byte, 0, 1+len(nonce)+keySize+k.aead.Overhead())
	wrapped = append(wrapped, wrapVersion)
	wrapped = append(wrapped, nonce...)
	wrapped = k.aead.Seal(wrapped, nonce, plaintext, k.wrapAAD(aad))
	return ports.DataKey{Plaintext: plaintext, Wrapped: wrapped}, nil
}

// UnwrapDataKey implements ports.KMS.
func (k *KMS) UnwrapDataKey(ctx context.Context, wrapped, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kms/local.UnwrapDataKey: %w", err)
	}
	nonceSize := k.aead.NonceSize()
	if len(wrapped) < 1+nonceSize || wrapped[0] != wrapVersion {
		return nil, fmt.Errorf("kms/local.UnwrapDataKey: %w: malformed wrapped key", domain.ErrKeyUnavailable)
	}
	nonce, ciphertext := wrapped[1:1+nonceSize], wrapped[1+nonceSize:]
	plaintext, err := k.aead.Open(nil, nonce, ciphertext, k.wrapAAD(aad))
	if err != nil || len(plaintext) != keySize {
		return nil, fmt.Errorf("kms/local.UnwrapDataKey: %w: key was not wrapped by %s for this owner",
			domain.ErrKeyUnavailable, k.keyID)
	}
	return plaintext, nil
}

// wrapAAD binds a wrapped key to this KMS key and to its owner. The key ID
// has a fixed length, so the concatenation is unambiguous.
func (k *KMS) wrapAAD(aad []byte) []byte {
	out := make([]byte, 0, len(k.keyID)+len(aad))
	out = append(out, k.keyID...)
	return append(out, aad...)
}
