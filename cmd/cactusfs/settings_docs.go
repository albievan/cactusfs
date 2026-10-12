package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// The formats "cactusfs config" prints the settings in.
const (
	formatEnv      = "env"
	formatMarkdown = "markdown"
)

const configUsage = `Usage of cactusfs config:
  cactusfs config [--format env|markdown]

Prints every setting of "cactusfs server", from the list the server itself
reads them by:

  env        an environment file with every variable commented out, to copy
             and fill in (deploy/cactusfs.env.example is this output)
  markdown   the table of flags, variables, defaults and meanings that
             README.md and docs/ARCHITECTURE.md carry

The server reads its variables from its environment and from no file: hand
it the file with whatever starts it (docker --env-file, a systemd
EnvironmentFile=, a shell that exports it). Secrets belong in a secret store,
not in the file.

Flags:
`

// runConfig runs "cactusfs config": it prints the settings of the server as
// an environment file or as a table.
func runConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cactusfs config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(stderr, configUsage)
		fs.PrintDefaults()
	}
	format := fs.String("format", formatEnv, "`format` to print: "+formatEnv+" or "+formatMarkdown)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "cactusfs config: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	var text string
	switch *format {
	case formatEnv:
		text = settingsEnvFile(serverSettings())
	case formatMarkdown:
		text = settingsMarkdown(serverSettings())
	default:
		_, _ = fmt.Fprintf(stderr, "cactusfs config: unknown format %q: want %s or %s\n", *format, formatEnv, formatMarkdown)
		return exitUsage
	}
	if _, err := io.WriteString(stdout, text); err != nil {
		_, _ = fmt.Fprintf(stderr, "cactusfs config: %v\n", err)
		return exitError
	}
	return exitOK
}

// holdsSecret reports whether the value of a setting is a secret or the path
// of a file that holds one: such a setting has no value in any document.
func (s settingSpec) holdsSecret() bool {
	return s.Kind == cluster.SettingSecret || s.Kind == cluster.SettingSecretFile
}

// plainUsage returns what --help says of the setting, without the backquotes
// that name its value there.
func (s settingSpec) plainUsage() string {
	_, usage := flag.UnquoteUsage(&flag.Flag{Usage: s.Usage})
	return usage
}

// settingsEnvFile returns every setting as an environment file in which
// every variable is commented out: copied as it is, it changes nothing. A
// secret, and the path of a file that holds one, has no value in it.
func settingsEnvFile(specs []settingSpec) string {
	var b strings.Builder
	b.WriteString(`# CactusFS - every setting of "cactusfs server", as an environment file.
#
# Printed by "cactusfs config --format env"; "make settings-docs" writes this
# file, and a test fails when the two differ. Do not edit it by hand.
#
# Copy it to a file of your own, outside the repository or under a name git
# ignores (.env, .env.*, *.env), and take the "#" off the lines you set.
# Every line is commented out, so the copy changes nothing until you do. The
# server reads its environment and no file: hand the file to whatever starts
# it -
#
#   docker run --env-file cactusfs.env ...
#   EnvironmentFile=/etc/cactusfs/cactusfs.env        (a systemd unit)
#   a shell, PowerShell: the loops of README.md, "An environment file"
#
# A line is NAME=value, and the value is everything after the first "=" as
# it stands: write it without quotes around it, on one line, with no comment
# after it. A shell that sources the file reads it otherwise - a space, a
# "$" or a quote in a value means something to it - which is why README.md
# has a loop instead.
#
# Each variable has a flag of the same name (CACTUSFS_S3_ADDR is --s3-addr),
# and the flag wins. A variable that is set to nothing counts as not set.
#
# SECRETS DO NOT BELONG IN THIS FILE. The lines of a secret are here so that
# the list is whole: give the value from a secret store (a Kubernetes Secret,
# the engine's secrets, the environment of the service manager). A line that
# names a file that holds a secret takes the path; keep that file outside the
# repository, readable by the node alone. Neither is ever committed.
`)
	for _, group := range settingGroups() {
		fmt.Fprintf(&b, "\n# ---- %s ----\n", group.Title)
		for _, spec := range specs {
			if spec.Group != group.Name {
				continue
			}
			b.WriteString("\n")
			envComment(&b, spec.plainUsage())
			value := spec.Default
			switch {
			case spec.Kind == cluster.SettingSecret:
				value = ""
				envComment(&b, "A secret, and so without a flag: set it from a secret store, never in a file that is committed.")
			case spec.Kind == cluster.SettingSecretFile:
				value = ""
				envComment(&b, "Flag: --"+spec.Name+". The path of a file that holds a secret: keep the file outside the repository; it is never committed.")
			case spec.Default != "":
				fmt.Fprintf(&b, "# Flag: --%s. Default: %s\n", spec.Name, spec.Default)
			default:
				fmt.Fprintf(&b, "# Flag: --%s. No default.\n", spec.Name)
			}
			fmt.Fprintf(&b, "#%s=%s\n", spec.Env, value)
		}
	}
	return b.String()
}

// The marks between which README.md and docs/ARCHITECTURE.md carry the table
// of settings.
const (
	settingsBegin = "<!-- settings:begin -->"
	settingsEnd   = "<!-- settings:end -->"
)

// markdownCell writes text so that it stays in one cell of a table and reads
// as it was written: outside a code span, a "|" would end the cell and a "<"
// begin a tag.
func markdownCell(text string) string {
	var b strings.Builder
	code := false
	for _, r := range text {
		switch {
		case r == '`':
			code = !code
		case r == '|', r == '<' && !code:
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// envCommentWidth is where a comment of the environment file is broken.
const envCommentWidth = 78

// envComment writes text as comment lines of the environment file, broken
// between words.
func envComment(b *strings.Builder, text string) {
	line := "#"
	for word := range strings.FieldsSeq(text) {
		if len(line)+1+len(word) > envCommentWidth && line != "#" {
			b.WriteString(line + "\n")
			line = "#"
		}
		line += " " + word
	}
	b.WriteString(line + "\n")
}

// settingsMarkdown returns the settings as tables, one for each group, with
// the flag, the variable, the default and the meaning of each.
func settingsMarkdown(specs []settingSpec) string {
	var b strings.Builder
	for i, group := range settingGroups() {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**%s**\n\n| Flag | Variable | Default | Meaning |\n|---|---|---|---|\n", group.Title)
		for _, spec := range specs {
			if spec.Group != group.Name {
				continue
			}
			flagCell := "— (no flag: a secret)"
			if spec.Flag {
				flagCell = "`--" + spec.Name + "`"
			}
			defaultCell := "—"
			switch {
			case spec.Default != "" && !spec.holdsSecret():
				defaultCell = "`" + spec.Default + "`"
			case spec.DefaultNote != "":
				defaultCell = spec.DefaultNote
			}
			meaning := spec.Doc
			if meaning == "" {
				meaning = spec.plainUsage()
			}
			switch spec.Kind {
			case cluster.SettingSecret:
				meaning += ". A secret: from the environment only, never shown"
			case cluster.SettingSecretFile:
				meaning += ". The file holds a secret: its path is not shown with the node's settings"
			case cluster.SettingPlain, cluster.SettingPath, cluster.SettingURL:
			}
			if spec.Scope == cluster.ScopeCluster {
				meaning += ". The same on every node"
			}
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", flagCell, spec.Env, markdownCell(defaultCell), markdownCell(meaning))
		}
	}
	return b.String()
}
