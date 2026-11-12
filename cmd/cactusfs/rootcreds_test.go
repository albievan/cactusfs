package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
)

// TestRootCredentials holds where the root credentials of a new deployment
// come from: each part from its variable or from its file, a file without
// the whitespace around it, and no file content in an error.
func TestRootCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const id, secret = "ROOTKEYFROMFILE", "root-secret-from-a-file"
	idFile, secretFile := filepath.Join(dir, "access-key"), filepath.Join(dir, "secret-key")
	// As a mounted Secret or `echo` leaves it: a line.
	writeTestFile(t, idFile, id+"\n")
	writeTestFile(t, secretFile, "  "+secret+"\r\n")
	empty, otherEmpty, huge := filepath.Join(dir, "empty"), filepath.Join(dir, "other-empty"), filepath.Join(dir, "huge")
	writeTestFile(t, empty, " \n\t")
	writeTestFile(t, otherEmpty, "")
	writeTestFile(t, huge, strings.Repeat("k", maxRootSecretFile+1))

	tests := []struct {
		name    string
		cfg     serverConfig
		want    *app.Credentials
		wantErr string
	}{
		{name: "none: a new deployment makes its own", cfg: serverConfig{}},
		{
			name: "from the variables",
			cfg:  serverConfig{secrets: secrets{rootAccessKey: "ROOTKEY", rootSecretKey: "s3cr3t"}},
			want: &app.Credentials{AccessKeyID: "ROOTKEY", SecretKey: "s3cr3t"},
		},
		{
			name: "from the files",
			cfg:  serverConfig{RootAccessKeyFile: idFile, RootSecretKeyFile: secretFile},
			want: &app.Credentials{AccessKeyID: id, SecretKey: secret},
		},
		{
			name: "the ID from its variable, the secret from its file",
			cfg:  serverConfig{secrets: secrets{rootAccessKey: "ROOTKEY"}, RootSecretKeyFile: secretFile},
			want: &app.Credentials{AccessKeyID: "ROOTKEY", SecretKey: secret},
		},
		{
			name: "the ID from its file, the secret from its variable",
			cfg:  serverConfig{secrets: secrets{rootSecretKey: "s3cr3t"}, RootAccessKeyFile: idFile},
			want: &app.Credentials{AccessKeyID: id, SecretKey: "s3cr3t"},
		},
		{
			name: "a missing access key file", cfg: serverConfig{RootAccessKeyFile: filepath.Join(dir, "absent"), RootSecretKeyFile: secretFile},
			wantErr: "root access key file",
		},
		{
			name: "a secret key file in a missing directory", cfg: serverConfig{RootAccessKeyFile: idFile, RootSecretKeyFile: filepath.Join(dir, "no", "dir", "s")},
			wantErr: "root secret key file",
		},
		// Empty, as a variable set to nothing is: none given, and a new
		// deployment makes its own - what the compose cluster relies on.
		{name: "two empty files", cfg: serverConfig{RootAccessKeyFile: empty, RootSecretKeyFile: otherEmpty}},
		{name: "an empty secret key file beside an access key", cfg: serverConfig{RootAccessKeyFile: idFile, RootSecretKeyFile: empty}, wantErr: "both or neither"},
		{
			name: "an empty access key file beside a secret key variable", cfg: serverConfig{RootAccessKeyFile: empty, secrets: secrets{rootSecretKey: "s3cr3t"}},
			wantErr: "both or neither",
		},
		{name: "a file too large to be a key", cfg: serverConfig{RootAccessKeyFile: huge, RootSecretKeyFile: secretFile}, wantErr: "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rootCredentials(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("rootCredentials() error = %v, want one that says %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), id) {
					t.Fatalf("the error tells what a file holds: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("rootCredentials() = %v", err)
			}
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("rootCredentials() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
