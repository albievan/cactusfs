package nodetls

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/albievan/cactusfs/internal/domain"
)

// maxPEMFile bounds a certificate, key or CA file.
const maxPEMFile = 1 << 20

// opener is where a certificate or key file is opened: a directory that is
// not left (*os.Root), or the file system as it is (anywhere).
type opener interface {
	Stat(name string) (os.FileInfo, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
}

// anywhere opens a path as the operating system resolves it: links are
// followed wherever they lead.
type anywhere struct{}

// Stat describes the file a path leads to, links followed.
func (anywhere) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

// OpenFile opens the file a path leads to, links followed.
func (anywhere) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	// The path is the operator's: a flag or an environment variable.
	return os.OpenFile(filepath.Clean(name), flag, perm)
}

// readConfined reads the certificate or the key of a node. The file is
// opened within its own directory: a link inside it is followed - a Secret
// volume is made of them - and one that leaves it is not.
func readConfined(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return readRegular(root, filepath.Base(path), path)
}

// readFollowing reads the certificate or the key of the listeners clients
// reach. Links are followed wherever they lead, as they were when
// tls.LoadX509KeyPair read these files: certbot's "live" directory links
// into "archive", and a file under /etc is often a link to another place.
func readFollowing(path string) ([]byte, error) {
	return readRegular(anywhere{}, path, path)
}

// readRegular reads a file whole, if it is a regular file of at most
// maxPEMFile bytes. Anything else - a pipe, a device, a directory - is
// refused before a byte is read: reading a pipe nobody writes to never ends.
// name is what from opens; path is what an error calls the file.
func readRegular(from opener, name, path string) ([]byte, error) {
	file, err := openForRead(from, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	// Asked of the file that was opened, not of its name: the name may be
	// given to another file in between.
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", domain.ErrInvalidArgument, path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPEMFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPEMFile {
		return nil, fmt.Errorf("%w: %s is too large to be a PEM file", domain.ErrInvalidArgument, path)
	}
	return data, nil
}

// readPair reads a certificate file and its key file with read.
func readPair(read func(path string) ([]byte, error), certFile, keyFile string) (certPEM, keyPEM []byte, err error) {
	if certPEM, err = read(certFile); err != nil {
		return nil, nil, err
	}
	if keyPEM, err = read(keyFile); err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}
