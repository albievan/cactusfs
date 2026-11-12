package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/albievan/cactusfs/internal/app"
)

// maxRootSecretFile bounds a file of the root credentials: an access key ID
// or a secret key is a few dozen bytes.
const maxRootSecretFile = 4 << 10

// rootCredentials returns the root credentials a new deployment is to begin
// with, as the operator gave them - each part from its variable or from its
// file - or nil when none were given and a new deployment makes its own.
//
// validateRootSecrets has made sure that both parts are named or neither. A
// file may still be empty, as a variable may: two parts that are both empty
// are no credentials, as two variables that are set to nothing are, and one
// empty part beside one that is not is refused. A file that cannot be read
// is an error; the content of a file is never in one.
func rootCredentials(cfg serverConfig) (*app.Credentials, error) {
	id, secret := cfg.secrets.rootAccessKey, cfg.secrets.rootSecretKey
	if cfg.RootAccessKeyFile != "" {
		read, err := readSecretFile(cfg.RootAccessKeyFile, "root access key file")
		if err != nil {
			return nil, err
		}
		id = string(read)
	}
	if cfg.RootSecretKeyFile != "" {
		read, err := readSecretFile(cfg.RootSecretKeyFile, "root secret key file")
		if err != nil {
			return nil, err
		}
		secret = string(read)
	}
	switch {
	case id == "" && secret == "":
		return nil, nil
	case id == "" || secret == "":
		return nil, errors.New("root credentials: the access key or the secret key is empty, and the other is not: give both or neither")
	}
	return &app.Credentials{AccessKeyID: id, SecretKey: secret}, nil
}

// readSecretFile returns what a file of one secret holds, without the
// whitespace around it, which a file written by an editor or by `echo` ends
// with; an empty file holds nothing. what names the file in errors; the
// content is never in one.
func readSecretFile(path, what string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxRootSecretFile+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	if len(data) > maxRootSecretFile {
		return nil, fmt.Errorf("read %s: file is too large to hold one key", what)
	}
	return bytes.TrimSpace(data), nil
}
