package cluster_test

import (
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// mark is what the tests hide in the places that must not be shown. It is
// put together here so that nothing else in the tree holds it.
const mark = "se" + "ntinel" + "-7f3a"

// allKinds is every kind, and one that is not a kind.
func allKinds() []cluster.SettingKind {
	return []cluster.SettingKind{
		cluster.SettingPlain, cluster.SettingPath, cluster.SettingURL,
		cluster.SettingSecret, cluster.SettingSecretFile, "duration",
	}
}

// shows reports whether a value of the kind may be shown at all.
func shows(kind cluster.SettingKind) bool {
	return kind == cluster.SettingPlain || kind == cluster.SettingPath || kind == cluster.SettingURL
}

func TestSettingEnumerationsValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		valid func(string) bool
		yes   []string
		no    []string
	}{
		{
			name:  "kind",
			valid: func(s string) bool { return cluster.SettingKind(s).Valid() },
			yes:   []string{"plain", "path", "url", "secret", "secret-file"},
			no:    []string{"", "Plain", "secret_file", "secretfile", "duration", " url"},
		},
		{
			name:  "scope",
			valid: func(s string) bool { return cluster.SettingScope(s).Valid() },
			yes:   []string{"node", "cluster"},
			no:    []string{"", "Node", "global", "cluster "},
		},
		{
			name:  "source",
			valid: func(s string) bool { return cluster.SettingSource(s).Valid() },
			yes:   []string{"default", "environment", "flag", "derived"},
			no:    []string{"", "env", "Flag", "file"},
		},
		{
			name:  "reason",
			valid: func(s string) bool { return cluster.AnswerReason(s).Valid() },
			yes:   []string{"", "unreachable", "timeout", "unsupported", "failed"},
			no:    []string{"none", "Timeout", "refused"},
		},
	}
	for _, tt := range tests {
		for _, s := range tt.yes {
			if !tt.valid(s) {
				t.Errorf("%s %q is not valid", tt.name, s)
			}
		}
		for _, s := range tt.no {
			if tt.valid(s) {
				t.Errorf("%s %q is valid", tt.name, s)
			}
		}
	}

	// The constants are the wire values: the contracts spell them so.
	wire := [][2]string{
		{string(cluster.SettingPlain), "plain"},
		{string(cluster.SettingPath), "path"},
		{string(cluster.SettingURL), "url"},
		{string(cluster.SettingSecret), "secret"},
		{string(cluster.SettingSecretFile), "secret-file"},
		{string(cluster.ScopeNode), "node"},
		{string(cluster.ScopeCluster), "cluster"},
		{string(cluster.SourceDefault), "default"},
		{string(cluster.SourceEnvironment), "environment"},
		{string(cluster.SourceFlag), "flag"},
		{string(cluster.SourceDerived), "derived"},
		{string(cluster.ReasonNone), ""},
		{string(cluster.ReasonUnreachable), "unreachable"},
		{string(cluster.ReasonTimeout), "timeout"},
		{string(cluster.ReasonUnsupported), "unsupported"},
		{string(cluster.ReasonFailed), "failed"},
		{cluster.CertificateS3, "s3"},
		{cluster.CertificateNode, "node"},
		{cluster.SettingKMSKeyID, "kms-key-id"},
		{cluster.SettingVersion, "version"},
	}
	for _, pair := range wire {
		got, want := pair[0], pair[1]
		if got != want {
			t.Errorf("constant %q, want %q", got, want)
		}
	}
	if cluster.ErasureSetUnknown != -1 {
		t.Errorf("ErasureSetUnknown = %d, want -1", cluster.ErasureSetUnknown)
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"https://vault.example:8200", "https://vault.example:8200"},
		{"https://vault.example:8200/v1/transit", "https://vault.example:8200/v1/transit"},
		{"https://user:" + mark + "@vault.example:8200/v1?token=" + mark + "#" + mark, "https://vault.example:8200/v1"},
		{"https://user@vault.example", "https://vault.example"},
		{"https://:" + mark + "@vault.example/", "https://vault.example/"},
		{"http://[::1]:4318/v1/traces?key=" + mark, "http://[::1]:4318/v1/traces"},
		{"https://u:" + mark + "@[2001:db8::1]:4318", "https://[2001:db8::1]:4318"},
		{"grpc+tls://collector:4317#" + mark, "grpc+tls://collector:4317"},
		// An OTLP endpoint is often given without a scheme.
		{"collector:4317", "collector:4317"},
		{"user:" + mark + "@collector:4317", "collector:4317"},
		{"collector:4317?key=" + mark, "collector:4317"},
		{"collector", "collector"},
		// A password that was not percent-encoded: none of "/", "?", "#"
		// and "@" in it lets a part of it through.
		{"https://user:" + mark + "/x@host/path", "https://host/path"},
		{"https://user:" + mark + "?x@host/path", "https://"},
		{"https://user:" + mark + "#x@host/path", "https://"},
		{"https://user:p@x?" + mark + "@host/path", "https://"},
		{"user:" + mark + "?x@collector:4317", ""},
		{"https://user:p@" + mark + "@host/path", "https://host/path"},
		{"user:" + mark + "/x@collector:4317", "collector:4317"},
		// What stands before "://" is not a scheme, so it is not kept.
		{"user:" + mark + "://x@host", "host"},
		{" https://user:" + mark + "@host", "host"},
		{"1https://user:" + mark + "@host", "host"},
		{"://user:" + mark + "@host", "host"},
		// An "@" in a query or a fragment: what follows it is as secret as
		// what stands before it.
		{"https://host/path?token=a@" + mark, "https://"},
		{"https://user:p@host/path#a@" + mark, "https://"},
		{"collector:4317?token=a@" + mark, ""},
		// The price of the strict rule for a URL that hid nothing.
		{"https://host/users/@me", "https://me"},
		{"https://host/?mail=a@b", "https://"},
		// Neither URL nor address.
		{"@", ""},
		{"?", ""},
		{"#", ""},
		{"https://", "https://"},
		{"https://user:" + mark + "@", "https://"},
		{"mailto:someone@example.org", "example.org"},
		{"\x00\xff@\xfe?", "\xfe"},
	}
	for _, tt := range tests {
		got := cluster.RedactURL(tt.in)
		if got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if strings.Contains(got, mark) {
			t.Errorf("RedactURL(%q) = %q shows the mark", tt.in, got)
		}
	}
}

// redactedURLHolds reports what is wrong with the redaction of raw, by the
// properties that hold for any input at all.
func redactedURLHolds(raw string) error {
	got := cluster.RedactURL(raw)
	switch {
	case len(got) > len(raw):
		return fmt.Errorf("RedactURL(%q) = %q is longer than what was given", raw, got)
	case strings.ContainsAny(got, "@?#"):
		return fmt.Errorf("RedactURL(%q) = %q keeps userinfo, a query or a fragment", raw, got)
	case cluster.RedactURL(got) != got:
		return fmt.Errorf("RedactURL(%q) = %q, and again %q", raw, got, cluster.RedactURL(got))
	}
	return nil
}

// TestRedactURLOfWellFormedURLs: of a URL as net/url writes it - whatever
// the user and the password, the query and the fragment - exactly scheme,
// host and path remain. The fragment is percent-encoded first: net/url
// writes an "@" in a fragment as it is, and RedactURL shows such a URL as
// its scheme alone.
func TestRedactURLOfWellFormedURLs(t *testing.T) {
	t.Parallel()
	schemes := []string{"http", "https", "grpc", "grpc+tls", "h2c"}
	hosts := []string{"vault", "vault.example:8200", "10.0.0.7:4317", "[::1]:4318", "[2001:db8::1]"}
	paths := []string{"", "/", "/v1/traces", "/a/b/c", "/with%20space", "/v1/transit/keys/cactusfs"}
	property := func(scheme, host, path uint8, user, password, key, value, fragment string) bool {
		given := url.URL{
			Scheme:   schemes[int(scheme)%len(schemes)],
			User:     url.UserPassword(user, mark+password),
			Host:     hosts[int(host)%len(hosts)],
			Path:     paths[int(path)%len(paths)],
			RawQuery: url.Values{key: {mark + value}}.Encode(),
			Fragment: mark + url.QueryEscape(fragment),
		}
		want := url.URL{Scheme: given.Scheme, Host: given.Host, Path: given.Path}
		raw := given.String()
		if err := redactedURLHolds(raw); err != nil {
			t.Log(err)
			return false
		}
		if got := cluster.RedactURL(raw); got != want.String() {
			t.Logf("RedactURL(%q) = %q, want %q", raw, got, want.String())
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// assembled puts a URL together from parts that may be anything, with the
// mark in the user, the password, the query and the fragment. It also
// returns what of it is not secret.
func assembled(scheme, user, password, host, path, query, fragment string) (raw, public string) {
	raw = scheme + "://" + mark + user + ":" + mark + password + "@" + host + path + "?" + mark + query + "#" + mark + fragment
	return raw, scheme + "://" + host + path
}

func FuzzRedactURL(f *testing.F) {
	for _, seed := range [][7]string{
		{"https", "user", "password", "vault.example:8200", "/v1/transit", "token=1", "top"},
		{"", "", "", "collector:4317", "", "", ""},
		{"http", "a@b", "c/d?e#f", "[::1]:4318", "/v1/traces", "x=a@b", "y@z"},
		{"u:p", "://", "://", "host", "/@", "?", "#"},
		{"grpc+tls", "\x00", "\xff\xfe", "h", "/p", "\n", "\r"},
		{" https", "u", "p://q", "h://i", "", "", ""},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4], seed[5], seed[6])
	}
	f.Fuzz(func(t *testing.T, scheme, user, password, host, path, query, fragment string) {
		// Each part alone, as garbage.
		for _, raw := range []string{scheme, user, password, host, path, query, fragment} {
			if err := redactedURLHolds(raw); err != nil {
				t.Fatal(err)
			}
		}
		raw, public := assembled(scheme, user, password, host, path, query, fragment)
		if err := redactedURLHolds(raw); err != nil {
			t.Fatal(err)
		}
		// Whatever the parts are, the mark stood only where a credential
		// stands. Parts that spell it themselves prove nothing.
		if strings.Contains(public, mark) {
			return
		}
		if got := cluster.RedactURL(raw); strings.Contains(got, mark) {
			t.Fatalf("RedactURL(%q) = %q shows the mark", raw, got)
		}
		// Without a scheme, as an OTLP endpoint is often given. The user is
		// not marked here: a user that ends in "://" is a scheme, by any
		// reading, and is kept.
		bare := user + strings.TrimPrefix(raw, scheme+"://"+mark+user)
		if strings.Contains(user, mark) {
			return
		}
		if err := redactedURLHolds(bare); err != nil {
			t.Fatal(err)
		}
		if got := cluster.RedactURL(bare); strings.Contains(got, mark) {
			t.Fatalf("RedactURL(%q) = %q shows the mark", bare, got)
		}
	})
}

// TestRedactDevice: the source of a mount is shown as it is unless it could
// name a credential, and then without it.
func TestRedactDevice(t *testing.T) {
	t.Parallel()
	tests := []struct{ source, want string }{
		{"", ""},
		{"/dev/sda1", "/dev/sda1"},
		{"/dev/mapper/vg0-data", "/dev/mapper/vg0-data"},
		{"/dev/disk/by-label/a#b?c", "/dev/disk/by-label/a#b?c"},
		{"overlay", "overlay"},
		{"tmpfs", "tmpfs"},
		{"UUID=0a3407de-014b-458b-b5c1-848e92a327a3", "UUID=0a3407de-014b-458b-b5c1-848e92a327a3"},
		{"filer.example:/export/data", "filer.example:/export/data"},
		{"[2001:db8::7]:/export", "[2001:db8::7]:/export"},
		{"//files/share", "//files/share"},
		{"//user:" + mark + "@files/share", "//files/share"},
		{"//files/share?password=" + mark, "//files/share"},
		{"//" + mark + "@//files/share", "//files/share"},
		{"//", "//"},
		{"user@host:/path", "host:/path"},
		{"user:" + mark + "@host:/path", "host:/path"},
		{"https://user:" + mark + "@host/dav", "https://host/dav"},
		{"https://host/dav?token=" + mark, "https://host/dav"},
		{"https://host/dav#" + mark, "https://host/dav"},
		{"s3fs://bucket?key=" + mark + "@x", "s3fs://"},
		{"/dev/odd@" + mark + "@name", "name"},
	}
	for _, tt := range tests {
		got := cluster.RedactDevice(tt.source)
		if got != tt.want {
			t.Errorf("RedactDevice(%q) = %q, want %q", tt.source, got, tt.want)
		}
		if strings.Contains(got, mark) {
			t.Errorf("RedactDevice(%q) = %q shows the mark", tt.source, got)
		}
	}
}

// redactedDeviceHolds reports what RedactDevice(source) breaks of its
// promises: nothing added, no "@" outside a scheme, and nothing changed by
// redacting again.
func redactedDeviceHolds(source string) error {
	got := cluster.RedactDevice(source)
	if len(got) > len(source) {
		return fmt.Errorf("RedactDevice(%q) = %q is longer than what it was given", source, got)
	}
	if strings.Contains(got, "@") {
		return fmt.Errorf("RedactDevice(%q) = %q names a user", source, got)
	}
	if again := cluster.RedactDevice(got); again != got {
		return fmt.Errorf("RedactDevice(%q) = %q, and redacted again %q", source, got, again)
	}
	if !strings.Contains(source, "@") && !strings.Contains(source, "://") && !strings.HasPrefix(source, "//") && got != source {
		return fmt.Errorf("RedactDevice(%q) = %q, though it names no credential", source, got)
	}
	return nil
}

func TestRedactDeviceProperties(t *testing.T) {
	t.Parallel()
	if err := quick.Check(func(source string) bool { return redactedDeviceHolds(source) == nil }, nil); err != nil {
		t.Error(err)
	}
	// Built of the pieces that matter, in every order of four.
	pieces := []string{"/", "//", "@", "://", "?", "#", "a", ":", "u:p"}
	var build func(prefix string, depth int)
	build = func(prefix string, depth int) {
		if err := redactedDeviceHolds(prefix); err != nil {
			t.Error(err)
		}
		if depth == 0 || t.Failed() {
			return
		}
		for _, piece := range pieces {
			build(prefix+piece, depth-1)
		}
	}
	build("", 4)
}

func FuzzRedactDevice(f *testing.F) {
	for _, seed := range []string{"/dev/sda1", "//u:p@h/s", "u@h:/p", "https://u:p@h/d?t=1#f", "//a@//b", "a@b://c@d", "\xff@\x00", "////", "//?@"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if err := redactedDeviceHolds(source); err != nil {
			t.Fatal(err)
		}
	})
}

// settingHolds reports what is wrong with the setting NewSetting makes of
// raw for the kind, by the properties that hold for any value at all.
func settingHolds(kind cluster.SettingKind, raw string) error {
	s := cluster.NewSetting("name", "CACTUSFS_NAME", "name", "group", kind, cluster.ScopeCluster, cluster.SourceEnvironment, raw)
	switch {
	case s.Name != "name" || s.Env != "CACTUSFS_NAME" || s.Flag != "name" || s.Group != "group" ||
		s.Kind != kind || s.Scope != cluster.ScopeCluster || s.Source != cluster.SourceEnvironment:
		return fmt.Errorf("NewSetting(%s) = %+v does not keep what describes it", kind, s)
	case s.Set != (raw != ""):
		return fmt.Errorf("NewSetting(%s, %q).Set = %v", kind, raw, s.Set)
	case !shows(kind) && s.Value != "":
		return fmt.Errorf("NewSetting(%s, %q) holds %q", kind, raw, s.Value)
	case len(s.Value) > cluster.MaxSettingValueBytes:
		return fmt.Errorf("NewSetting(%s) holds %d bytes", kind, len(s.Value))
	case !utf8.ValidString(s.Value):
		return fmt.Errorf("NewSetting(%s, %q) holds %q, which is not UTF-8", kind, raw, s.Value)
	case kind == cluster.SettingURL && strings.ContainsAny(s.Value, "@?#"):
		return fmt.Errorf("NewSetting(%s, %q) holds %q", kind, raw, s.Value)
	case s.Redacted() != s:
		return fmt.Errorf("NewSetting(%s, %q) = %+v, redacted again %+v", kind, raw, s, s.Redacted())
	}
	whole := utf8.ValidString(raw) && len(raw) <= cluster.MaxSettingValueBytes
	if (kind == cluster.SettingPlain || kind == cluster.SettingPath) && whole && s.Value != raw {
		return fmt.Errorf("NewSetting(%s, %q) holds %q", kind, raw, s.Value)
	}
	if kind == cluster.SettingURL && whole && s.Value != cluster.RedactURL(raw) {
		return fmt.Errorf("NewSetting(%s, %q) holds %q, want %q", kind, raw, s.Value, cluster.RedactURL(raw))
	}
	return nil
}

// secretStaysOut reports whether anything of a secret given by hand - in a
// literal, which NewSetting would not have let through - is left after
// Redacted and after Normalized.
func secretStaysOut(kind cluster.SettingKind, secret string, set bool) error {
	byHand := cluster.Setting{Name: "name", Kind: kind, Scope: cluster.ScopeNode, Source: cluster.SourceFlag, Set: set, Value: mark + secret}
	redacted := byHand.Redacted()
	normalized := cluster.NodeSettings{Settings: []cluster.Setting{byHand}}.Normalized().Settings[0]
	for how, got := range map[string]cluster.Setting{"Redacted": redacted, "Normalized": normalized} {
		switch {
		case got.Value != "" || strings.Contains(fmt.Sprintf("%+v", got), mark):
			return fmt.Errorf("%s of a %s by hand = %+v", how, kind, got)
		case !got.Set:
			return fmt.Errorf("%s of a %s by hand that holds a value is not set", how, kind)
		case got.Redacted() != got:
			return fmt.Errorf("%s of a %s by hand = %+v, and again %+v", how, kind, got, got.Redacted())
		}
	}
	if made := cluster.NewSetting("name", "", "", "", kind, cluster.ScopeNode, cluster.SourceFlag, mark+secret); made != redacted {
		return fmt.Errorf("NewSetting(%s) = %+v, Redacted of the same by hand = %+v", kind, made, redacted)
	}
	return nil
}

func TestNewSetting(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", cluster.MaxSettingValueBytes+1)
	tests := []struct {
		name    string
		kind    cluster.SettingKind
		raw     string
		want    string
		wantSet bool
	}{
		{name: "plain", kind: cluster.SettingPlain, raw: ":9000", want: ":9000", wantSet: true},
		{name: "plain unset", kind: cluster.SettingPlain},
		{name: "plain that looks like a URL is kept", kind: cluster.SettingPlain, raw: "a@b?c#d", want: "a@b?c#d", wantSet: true},
		{name: "path", kind: cluster.SettingPath, raw: "/data/1,/data/2", want: "/data/1,/data/2", wantSet: true},
		{name: "path unset", kind: cluster.SettingPath},
		{name: "url", kind: cluster.SettingURL, raw: "https://u:" + mark + "@vault:8200/v1?t=" + mark, want: "https://vault:8200/v1", wantSet: true},
		{name: "url unset", kind: cluster.SettingURL},
		{name: "url that is all query is set and shows nothing", kind: cluster.SettingURL, raw: "?" + mark, wantSet: true},
		{name: "secret", kind: cluster.SettingSecret, raw: mark, wantSet: true},
		{name: "secret unset", kind: cluster.SettingSecret},
		{name: "secret file", kind: cluster.SettingSecretFile, raw: "/run/secrets/" + mark, wantSet: true},
		{name: "secret file unset", kind: cluster.SettingSecretFile},
		{name: "unknown kind is a secret", kind: "duration", raw: mark, wantSet: true},
		{name: "no kind is a secret", raw: mark, wantSet: true},
		{name: "at the bound", kind: cluster.SettingPlain, raw: long[1:], want: long[1:], wantSet: true},
		{
			name: "over the bound", kind: cluster.SettingPlain, raw: long, wantSet: true,
			want: long[:cluster.MaxSettingValueBytes-len(cluster.TruncationMarker)] + cluster.TruncationMarker,
		},
		{
			// After one byte every "é" begins at an odd one, and the cut
			// would fall on an even: it moves back to where the rune begins.
			name: "cut at a rune boundary", kind: cluster.SettingPath, raw: "a" + strings.Repeat("é", cluster.MaxSettingValueBytes), wantSet: true,
			want: "a" + strings.Repeat("é", (cluster.MaxSettingValueBytes-len(cluster.TruncationMarker)-1)/2) + cluster.TruncationMarker,
		},
		{
			name: "long URL", kind: cluster.SettingURL, raw: "https://u:" + mark + "@h/" + long + "?" + mark, wantSet: true,
			want: ("https://h/" + long)[:cluster.MaxSettingValueBytes-len(cluster.TruncationMarker)] + cluster.TruncationMarker,
		},
		{name: "not UTF-8", kind: cluster.SettingPlain, raw: "a\xffb", want: "a\uFFFDb", wantSet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cluster.NewSetting("n", "CACTUSFS_N", "n", "g", tt.kind, cluster.ScopeNode, cluster.SourceFlag, tt.raw)
			want := cluster.Setting{
				Name: "n", Env: "CACTUSFS_N", Flag: "n", Group: "g",
				Kind: tt.kind, Scope: cluster.ScopeNode, Source: cluster.SourceFlag, Set: tt.wantSet, Value: tt.want,
			}
			if got != want {
				t.Errorf("NewSetting = %+v, want %+v", got, want)
			}
			if err := settingHolds(tt.kind, tt.raw); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestSettingRedactionProperties: for every kind and any value, what
// NewSetting keeps is bounded, redacted and stable; and a secret put into a
// Setting by hand does not outlive Redacted or Normalized.
func TestSettingRedactionProperties(t *testing.T) {
	t.Parallel()
	for _, kind := range allKinds() {
		property := func(raw string, repeat uint16, set bool) bool {
			for _, value := range []string{raw, strings.Repeat(raw, int(repeat)%64), mark + raw, "https://u:" + raw + "@h/p?" + raw} {
				if err := settingHolds(kind, value); err != nil {
					t.Log(err)
					return false
				}
			}
			if !shows(kind) {
				if err := secretStaysOut(kind, raw, set); err != nil {
					t.Log(err)
					return false
				}
			}
			return true
		}
		if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

func TestRedactedOfSettingsMadeByHand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   cluster.Setting
		want cluster.Setting
	}{
		{
			name: "secret with a value",
			in:   cluster.Setting{Name: "kms-passphrase", Kind: cluster.SettingSecret, Value: mark},
			want: cluster.Setting{Name: "kms-passphrase", Kind: cluster.SettingSecret, Set: true},
		},
		{
			name: "secret file with a path",
			in:   cluster.Setting{Name: "vault-token-file", Kind: cluster.SettingSecretFile, Set: true, Value: "/run/" + mark},
			want: cluster.Setting{Name: "vault-token-file", Kind: cluster.SettingSecretFile, Set: true},
		},
		{
			name: "secret that is set stays set",
			in:   cluster.Setting{Kind: cluster.SettingSecret, Set: true},
			want: cluster.Setting{Kind: cluster.SettingSecret, Set: true},
		},
		{
			name: "secret that is not set stays so",
			in:   cluster.Setting{Kind: cluster.SettingSecret},
			want: cluster.Setting{Kind: cluster.SettingSecret},
		},
		{
			name: "no kind",
			in:   cluster.Setting{Name: "x", Value: mark},
			want: cluster.Setting{Name: "x", Set: true},
		},
		{
			name: "URL with a credential",
			in:   cluster.Setting{Kind: cluster.SettingURL, Value: "https://u:" + mark + "@h/p?k=" + mark + "#" + mark},
			want: cluster.Setting{Kind: cluster.SettingURL, Set: true, Value: "https://h/p"},
		},
		{
			name: "plain with a value and not marked set",
			in:   cluster.Setting{Kind: cluster.SettingPlain, Value: "eu-west-1"},
			want: cluster.Setting{Kind: cluster.SettingPlain, Set: true, Value: "eu-west-1"},
		},
		{
			name: "plain too long",
			in:   cluster.Setting{Kind: cluster.SettingPlain, Value: strings.Repeat("b", 3*cluster.MaxSettingValueBytes)},
			want: cluster.Setting{
				Kind: cluster.SettingPlain, Set: true,
				Value: strings.Repeat("b", cluster.MaxSettingValueBytes-len(cluster.TruncationMarker)) + cluster.TruncationMarker,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.in.Redacted()
			if got != tt.want {
				t.Errorf("Redacted = %+v, want %+v", got, tt.want)
			}
			if got.Redacted() != got {
				t.Errorf("Redacted twice = %+v, once %+v", got.Redacted(), got)
			}
			if strings.Contains(fmt.Sprintf("%+v", got), mark) {
				t.Errorf("Redacted = %+v shows the mark", got)
			}
		})
	}
}

func FuzzNewSetting(f *testing.F) {
	for _, seed := range []string{
		"", ":9000", "/data/1", "https://u:p@vault:8200/v1?x=1#y", "user:pw@collector:4317",
		"\xff\xfe", strings.Repeat("é", cluster.MaxSettingValueBytes), "a@b?c#d", "://", cluster.TruncationMarker,
	} {
		for pick := range uint8(6) {
			f.Add(seed, pick)
		}
	}
	f.Fuzz(func(t *testing.T, raw string, pick uint8) {
		kinds := allKinds()
		kind := kinds[int(pick)%len(kinds)]
		for _, value := range []string{raw, mark + raw, strings.Repeat(raw+"/", 1+int(pick)%40)} {
			if err := settingHolds(kind, value); err != nil {
				t.Fatal(err)
			}
		}
		if shows(kind) {
			return
		}
		if err := secretStaysOut(kind, raw, pick%2 == 0); err != nil {
			t.Fatal(err)
		}
	})
}

// someNode returns settings of a node with everything filled in and nothing
// in order.
func someNode() cluster.NodeSettings {
	at := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	return cluster.NodeSettings{
		NodeID:    "n1",
		Roles:     []string{"storage", "gateway"},
		Labels:    map[string]string{"zone": "a", "rack": "r1"},
		Build:     cluster.Build{Version: "1.4.0", Build: "112", Commit: "381eb67"},
		StartedAt: at,
		KMSKeyID:  "cactusfs",
		Settings: []cluster.Setting{
			cluster.NewSetting("s3-addr", "CACTUSFS_S3_ADDR", "s3-addr", "Listeners", cluster.SettingPlain, cluster.ScopeNode, cluster.SourceDefault, ":9000"),
			{Name: "kms-passphrase", Env: "CACTUSFS_KMS_PASSPHRASE", Kind: cluster.SettingSecret, Scope: cluster.ScopeCluster, Value: mark},
			{Name: "otlp-endpoint", Kind: cluster.SettingURL, Scope: cluster.ScopeCluster, Value: "https://u:" + mark + "@otel:4318/v1?k=" + mark},
			{Name: "data", Env: "B", Kind: cluster.SettingPath, Value: "/data/2"},
			{Name: "data", Env: "A", Flag: "z", Kind: cluster.SettingPath, Value: "/data/1"},
			{Name: "data", Env: "A", Flag: "y", Kind: cluster.SettingPath, Value: "/data/0"},
		},
		Drives: []cluster.DriveSettings{
			{ID: "d2", Volume: cluster.Volume{Directory: "/data/2", Label: "cactusfs_node1-data2", TotalBytes: 100, FreeBytes: 40}, Online: true, ErasureSet: 0},
			{
				ID: "d1", Online: false, ErasureSet: cluster.ErasureSetUnknown,
				Volume: cluster.Volume{Directory: "/data/1", Filesystem: cluster.Filesystem{Type: "xfs", Device: "/dev/sdb1", MountPoint: "/data"}},
			},
		},
		Metadata: cluster.Volume{Directory: "/data/1/meta", TotalBytes: 100, FreeBytes: 40},
		Certificates: []cluster.Certificate{
			{Use: cluster.CertificateS3, Subject: "CN=s3", DNSNames: []string{"s3.example"}, NotAfter: at.Add(48 * time.Hour)},
			{Use: cluster.CertificateNode, Subject: "CN=n1", Issuer: "CN=ca", DNSNames: []string{"n1"}, IPAddresses: []string{"10.0.0.1"}, NotBefore: at, NotAfter: at.Add(48 * time.Hour)},
			{Use: cluster.CertificateNode, Subject: "CN=n1", NotAfter: at.Add(24 * time.Hour)},
			{Use: cluster.CertificateNode, Subject: "CN=a1"},
		},
	}
}

func TestNodeSettingsNormalized(t *testing.T) {
	t.Parallel()
	given := someNode()
	got := given.Normalized()

	var names, drives, certs []string
	for _, s := range got.Settings {
		names = append(names, s.Name+"="+s.Value)
	}
	for _, d := range got.Drives {
		drives = append(drives, d.ID)
	}
	for _, c := range got.Certificates {
		certs = append(certs, c.Use+" "+c.Subject+" "+c.NotAfter.Format(time.DateOnly))
	}
	wantNames := []string{"data=/data/0", "data=/data/1", "data=/data/2", "kms-passphrase=", "otlp-endpoint=https://otel:4318/v1", "s3-addr=:9000"}
	if !slices.Equal(names, wantNames) {
		t.Errorf("settings %q, want %q", names, wantNames)
	}
	if want := []string{"d1", "d2"}; !slices.Equal(drives, want) {
		t.Errorf("drives %q, want %q", drives, want)
	}
	wantCerts := []string{"node CN=a1 0001-01-01", "node CN=n1 2026-10-13", "node CN=n1 2026-10-14", "s3 CN=s3 2026-10-14"}
	if !slices.Equal(certs, wantCerts) {
		t.Errorf("certificates %q, want %q", certs, wantCerts)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), mark) {
		t.Errorf("Normalized shows the mark: %+v", got)
	}
	for _, s := range got.Settings {
		if (s.Name == "kms-passphrase" || s.Name == "otlp-endpoint") && !s.Set {
			t.Errorf("%s holds a value and is not set", s.Name)
		}
	}

	// Everything else is as it was given, and normalizing again changes
	// nothing.
	if got.NodeID != given.NodeID || got.Build != given.Build || !got.StartedAt.Equal(given.StartedAt) ||
		got.KMSKeyID != given.KMSKeyID || got.Metadata != given.Metadata ||
		!slices.Equal(got.Roles, given.Roles) || !reflect.DeepEqual(got.Labels, given.Labels) {
		t.Errorf("Normalized = %+v, of %+v", got, given)
	}
	if got.Drives[0] != given.Drives[1] || got.Drives[1] != given.Drives[0] {
		t.Errorf("drives %+v, of %+v", got.Drives, given.Drives)
	}
	if again := got.Normalized(); !reflect.DeepEqual(again, got) {
		t.Errorf("Normalized twice = %+v, once %+v", again, got)
	}

	// Nothing of the result is the receiver's: writing to it leaves the
	// receiver as it was.
	want := someNode()
	got.Roles[0] = "x"
	got.Labels["zone"] = "x"
	got.Labels["new"] = "x"
	got.Settings[0].Value = "x"
	got.Drives[0].ID = "x"
	got.Certificates[0].Subject = "x"
	for i := range got.Certificates {
		for j := range got.Certificates[i].DNSNames {
			got.Certificates[i].DNSNames[j] = "x"
		}
		for j := range got.Certificates[i].IPAddresses {
			got.Certificates[i].IPAddresses[j] = "x"
		}
	}
	if !reflect.DeepEqual(given, want) {
		t.Errorf("Normalized shares with its receiver: %+v, was %+v", given, want)
	}

	// Nothing stays nothing.
	if empty := (cluster.NodeSettings{}).Normalized(); !reflect.DeepEqual(empty, cluster.NodeSettings{}) {
		t.Errorf("Normalized of nothing = %+v", empty)
	}
}

// answer returns the answer of a node of version 1.4.0-112 and KMS key
// "cactusfs" with the settings given.
func answer(id string, settings ...cluster.Setting) cluster.NodeAnswer {
	return cluster.NodeAnswer{
		NodeID: id, Answered: true,
		Settings: cluster.NodeSettings{
			NodeID: id, KMSKeyID: "cactusfs", Build: cluster.Build{Version: "1.4.0", Build: "112", Commit: "c-" + id},
			Settings: settings,
		},
	}
}

// with returns the answer changed.
func with(a cluster.NodeAnswer, change func(*cluster.NodeAnswer)) cluster.NodeAnswer {
	change(&a)
	return a
}

// A few settings for the tables: by scope, kind and source.
func region(v string) cluster.Setting {
	return cluster.NewSetting("region", "CACTUSFS_REGION", "region", "S3", cluster.SettingPlain, cluster.ScopeCluster, cluster.SourceEnvironment, v)
}

func addr(v string) cluster.Setting {
	return cluster.NewSetting("s3-addr", "CACTUSFS_S3_ADDR", "s3-addr", "Listeners", cluster.SettingPlain, cluster.ScopeNode, cluster.SourceFlag, v)
}

func phrase(v string) cluster.Setting {
	return cluster.NewSetting("kms-passphrase", "CACTUSFS_KMS_PASSPHRASE", "", "KMS", cluster.SettingSecret, cluster.ScopeCluster, cluster.SourceEnvironment, v)
}

func vault(v string) cluster.Setting {
	return cluster.NewSetting("vault-addr", "CACTUSFS_VAULT_ADDR", "vault-addr", "KMS", cluster.SettingURL, cluster.ScopeCluster, cluster.SourceEnvironment, v)
}

func grace(source cluster.SettingSource, v string) cluster.Setting {
	return cluster.NewSetting("gc-grace", "CACTUSFS_GC_GRACE", "gc-grace", "Storage", cluster.SettingPlain, cluster.ScopeCluster, source, v)
}

func TestSettingMismatches(t *testing.T) {
	t.Parallel()
	type values = []cluster.MismatchValue
	tests := []struct {
		name    string
		answers []cluster.NodeAnswer
		want    []cluster.Mismatch
	}{
		{name: "no answers"},
		{
			name:    "a single node differs from nobody",
			answers: []cluster.NodeAnswer{answer("n1", region("eu"), phrase(mark))},
		},
		{
			name: "all equal",
			answers: []cluster.NodeAnswer{
				answer("n1", region("eu"), phrase("a"+mark), vault("https://vault:8200")),
				answer("n2", region("eu"), phrase("b"+mark), vault("https://vault:8200")),
				answer("n3", region("eu"), phrase("c"+mark), vault("https://vault:8200")),
			},
		},
		{
			name: "one node differs",
			answers: []cluster.NodeAnswer{
				answer("n3", region("eu")), answer("n1", region("eu")), answer("n2", region("us")),
			},
			want: []cluster.Mismatch{{Setting: "region", Values: values{{"n1", "eu"}, {"n2", "us"}, {"n3", "eu"}}}},
		},
		{
			name: "settings of a node may differ",
			answers: []cluster.NodeAnswer{
				answer("n1", addr(":9000"), region("eu")), answer("n2", addr(":9010"), region("eu")),
			},
		},
		{
			name: "secrets differ by whether they are set, and only so",
			answers: []cluster.NodeAnswer{
				answer("n1", phrase("a"+mark)), answer("n2", phrase("")), answer("n3", phrase("b"+mark)),
			},
			want: []cluster.Mismatch{{Setting: "kms-passphrase", Values: values{{"n1", "set"}, {"n2", "not set"}, {"n3", "set"}}}},
		},
		{
			name: "a secret made by hand is compared and shown the same way",
			answers: []cluster.NodeAnswer{
				answer("n1", cluster.Setting{Name: "token", Kind: cluster.SettingSecret, Scope: cluster.ScopeCluster, Value: mark}),
				answer("n2", cluster.Setting{Name: "token", Kind: cluster.SettingSecret, Scope: cluster.ScopeCluster}),
			},
			want: []cluster.Mismatch{{Setting: "token", Values: values{{"n1", "set"}, {"n2", "not set"}}}},
		},
		{
			name: "URLs are compared without their credentials",
			answers: []cluster.NodeAnswer{
				answer("n1", vault("https://a:"+mark+"@vault:8200")), answer("n2", vault("https://b:"+mark+"@vault:8200?x="+mark)),
			},
		},
		{
			name: "URLs that differ",
			answers: []cluster.NodeAnswer{
				answer("n1", vault("https://a:"+mark+"@vault-1:8200")), answer("n2", vault("https://vault-2:8200")),
			},
			want: []cluster.Mismatch{{Setting: "vault-addr", Values: values{{"n1", "https://vault-1:8200"}, {"n2", "https://vault-2:8200"}}}},
		},
		{
			name: "nodes that did not answer are left out",
			answers: []cluster.NodeAnswer{
				answer("n1", region("eu")), answer("n2", region("eu")),
				with(answer("n3", region("us")), func(a *cluster.NodeAnswer) { a.Answered, a.Reason = false, cluster.ReasonTimeout }),
				{NodeID: "n4", Reason: cluster.ReasonUnreachable},
				with(answer("n5"), func(a *cluster.NodeAnswer) {
					a.Answered, a.Reason = false, cluster.ReasonUnsupported
					a.Settings.Build.Version, a.Settings.KMSKeyID = "1.3.0", "other"
				}),
			},
		},
		{
			name: "one answer among those that did not",
			answers: []cluster.NodeAnswer{
				answer("n1", region("eu")), {NodeID: "n2", Reason: cluster.ReasonFailed},
			},
		},
		{
			name: "a setting an older node does not know, at its default on the others",
			answers: []cluster.NodeAnswer{
				answer("n1", grace(cluster.SourceDefault, "24h")), answer("n2", grace(cluster.SourceDefault, "24h")), answer("n3"),
			},
		},
		{
			name: "a setting an older node does not know, not set on the others",
			answers: []cluster.NodeAnswer{
				answer("n1", grace(cluster.SourceEnvironment, "")), answer("n2"),
			},
		},
		{
			name: "a setting an older node does not know, chosen on another",
			answers: []cluster.NodeAnswer{
				answer("n1"), answer("n2", grace(cluster.SourceFlag, "48h")), answer("n3", grace(cluster.SourceDefault, "48h")),
			},
			want: []cluster.Mismatch{{Setting: "gc-grace", Values: values{{"n1", ""}, {"n2", "48h"}, {"n3", "48h"}}}},
		},
		{
			name: "a setting an older node does not know, in which the others differ",
			answers: []cluster.NodeAnswer{
				answer("n1", grace(cluster.SourceDefault, "24h")), answer("n2"), answer("n3", grace(cluster.SourceDefault, "12h")),
			},
			want: []cluster.Mismatch{{Setting: "gc-grace", Values: values{{"n1", "24h"}, {"n2", ""}, {"n3", "12h"}}}},
		},
		{
			name: "a secret an older node does not know, set on another",
			answers: []cluster.NodeAnswer{
				answer("n1"), answer("n2", phrase(mark)),
			},
			want: []cluster.Mismatch{{Setting: "kms-passphrase", Values: values{{"n1", "not set"}, {"n2", "set"}}}},
		},
		{
			name: "a secret an older node does not know, set on nobody",
			answers: []cluster.NodeAnswer{
				answer("n1"), answer("n2", phrase("")),
			},
		},
		{
			name: "a URL that shows as nothing is as good as none",
			answers: []cluster.NodeAnswer{
				answer("n1"), answer("n2", vault("?"+mark)),
			},
		},
		{
			name: "a setting counts where it is of the cluster",
			answers: []cluster.NodeAnswer{
				answer("n1", addr(":9000")),
				answer("n2", scoped(addr(":9010"), cluster.ScopeCluster)),
				answer("n3", scoped(addr(":9020"), cluster.ScopeCluster)),
			},
			want: []cluster.Mismatch{{Setting: "s3-addr", Values: values{{"n1", ""}, {"n2", ":9010"}, {"n3", ":9020"}}}},
		},
		{
			name: "the KMS key",
			answers: []cluster.NodeAnswer{
				answer("n2"), with(answer("n1"), func(a *cluster.NodeAnswer) { a.Settings.KMSKeyID = "other" }),
			},
			want: []cluster.Mismatch{{Setting: "kms-key-id", Values: values{{"n1", "other"}, {"n2", "cactusfs"}}}},
		},
		{
			name: "the version, by release and by build and not by commit",
			answers: []cluster.NodeAnswer{
				answer("n1"),
				with(answer("n2"), func(a *cluster.NodeAnswer) { a.Settings.Build.Build = "113" }),
				with(answer("n3"), func(a *cluster.NodeAnswer) { a.Settings.Build.Version = "1.5.0" }),
			},
			want: []cluster.Mismatch{{Setting: "version", Values: values{{"n1", "1.4.0-112"}, {"n2", "1.4.0-113"}, {"n3", "1.5.0-112"}}}},
		},
		{
			name: "settings named as what is compared besides are left out",
			answers: []cluster.NodeAnswer{
				answer("n1", named(region("a"), "version"), named(region("a"), "kms-key-id")),
				answer("n2", named(region("b"), "version"), named(region("b"), "kms-key-id")),
			},
		},
		{
			name: "of two settings with one name the first in order counts",
			answers: []cluster.NodeAnswer{
				answer("n1", fromEnv(region("b"), "B"), fromEnv(region("a"), "A")),
				answer("n2", fromEnv(region("a"), "A"), fromEnv(region("c"), "C")),
			},
		},
		{
			name: "by setting, then by node",
			answers: []cluster.NodeAnswer{
				with(answer("n2", vault("https://v2"), region("us"), phrase(""), addr(":1")), func(a *cluster.NodeAnswer) {
					a.Settings.Build.Version, a.Settings.KMSKeyID = "1.5.0", "k2"
				}),
				answer("n10", region("eu"), vault("https://v1"), phrase(mark), addr(":2")),
				answer("n1", phrase(mark), vault("https://v1"), region("eu"), addr(":3")),
			},
			want: []cluster.Mismatch{
				{Setting: "kms-key-id", Values: values{{"n1", "cactusfs"}, {"n10", "cactusfs"}, {"n2", "k2"}}},
				{Setting: "kms-passphrase", Values: values{{"n1", "set"}, {"n10", "set"}, {"n2", "not set"}}},
				{Setting: "region", Values: values{{"n1", "eu"}, {"n10", "eu"}, {"n2", "us"}}},
				{Setting: "vault-addr", Values: values{{"n1", "https://v1"}, {"n10", "https://v1"}, {"n2", "https://v2"}}},
				{Setting: "version", Values: values{{"n1", "1.4.0-112"}, {"n10", "1.4.0-112"}, {"n2", "1.5.0-112"}}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cluster.SettingMismatches(tt.answers)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SettingMismatches = %+v, want %+v", got, tt.want)
			}
			// The order of the answers does not matter.
			backwards := slices.Clone(tt.answers)
			slices.Reverse(backwards)
			if again := cluster.SettingMismatches(backwards); !reflect.DeepEqual(again, got) {
				t.Errorf("SettingMismatches of the answers backwards = %+v, forwards %+v", again, got)
			}
			if strings.Contains(fmt.Sprintf("%+v", got), mark) {
				t.Errorf("SettingMismatches shows the mark: %+v", got)
			}
		})
	}
}

// scoped returns the setting with another scope, named with another name,
// fromEnv with another variable.
func scoped(s cluster.Setting, scope cluster.SettingScope) cluster.Setting {
	s.Scope = scope
	return s
}

func named(s cluster.Setting, name string) cluster.Setting {
	s.Name = name
	return s
}

func fromEnv(s cluster.Setting, env string) cluster.Setting {
	s.Env = env
	return s
}

// TestSettingMismatchesProperties: whatever the nodes hold, a mismatch names
// a setting once, lists every node that answered once and in order, holds at
// least two different values, never a secret, and is the same whatever the
// order of the answers.
func TestSettingMismatchesProperties(t *testing.T) {
	t.Parallel()
	kinds := allKinds()
	property := func(raw []uint16) bool {
		var answers []cluster.NodeAnswer
		answered := 0
		for i, r := range raw {
			if i == 12 {
				break
			}
			a := answer(fmt.Sprintf("n%02d", i))
			a.Answered = r%5 != 0
			a.Settings.KMSKeyID = fmt.Sprintf("k%d", r%7/6)
			a.Settings.Build.Build = fmt.Sprintf("%d", r%11/10)
			for j := range 6 {
				v := int(r>>j) % 4
				if v == 3 {
					continue
				}
				value := ""
				if v > 0 {
					value = fmt.Sprintf("%s-%d", mark, v)
				}
				scope := cluster.ScopeCluster
				if j == 5 {
					scope = cluster.ScopeNode
				}
				a.Settings.Settings = append(a.Settings.Settings, cluster.Setting{
					Name: fmt.Sprintf("s%d", j), Kind: kinds[j%len(kinds)], Scope: scope, Source: cluster.SourceFlag, Set: v > 0, Value: value,
				})
			}
			if a.Answered {
				answered++
			}
			answers = append(answers, a)
		}
		got := cluster.SettingMismatches(answers)
		backwards := slices.Clone(answers)
		slices.Reverse(backwards)
		if !reflect.DeepEqual(cluster.SettingMismatches(backwards), got) {
			t.Logf("the order of the answers matters: %+v", answers)
			return false
		}
		if answered < 2 && got != nil {
			t.Logf("%d answers and mismatches %+v", answered, got)
			return false
		}
		last := ""
		for _, m := range got {
			distinct := map[string]bool{}
			node := ""
			for _, v := range m.Values {
				distinct[v.Value] = true
				// s3 and s4 are secrets, s5 is of the node.
				secret := m.Setting == "s3" || m.Setting == "s4"
				if v.NodeID <= node || (secret && v.Value != "set" && v.Value != "not set") {
					t.Logf("mismatch %+v", m)
					return false
				}
				node = v.NodeID
			}
			if m.Setting <= last || m.Setting == "s5" || len(m.Values) != answered || len(distinct) < 2 {
				t.Logf("mismatch %+v among %d answers, after %q", m, answered, last)
				return false
			}
			last = m.Setting
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}
