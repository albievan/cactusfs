//go:build deployscript

package deploy_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The images the script makes (PLAN 10.14, deploy/lib/image.sh): an image
// of binaries made with the time of their commit, so that the same
// binaries make the same image; the tags it makes on an engine that lacked
// them recorded in the entry's file "images"; the tag a replaced node ran
// removed after an upgrade, and every recorded tag at a purge, each only
// when no container of its engine names it; a tag the script did not
// record never removed. And the warning of an install whose S3 is
// published beyond the loopback with TLS_HOSTS empty.

// commitTime is what git says is the time of the commit imgCommit.
const commitTime = "1763900000"

// commitTimeRule is git's answer of the time of the commit given.
func commitTimeRule(commit string) rule {
	return rule{Tool: "git", Contains: []string{"show", "-s", "--format=%ct", commit + "^{commit}"}, Stdout: commitTime + "\n"}
}

// tagAbsent says the engine of the tool holds no image of the tag of f
// until it is tagged: the tag is the script's to record.
func tagAbsent(tool string, f imgFix) rule {
	return rule{Tool: tool, Contains: []string{"image", "inspect", "--format", "{{.Id}}", f.ref()}, Until: []string{"tag", f.ref()}, Stderr: noSuchImage(tool, f.ref()), Exit: 1}
}

// asRun is a tag as the engine of the tool writes it of a container: Podman
// names an image it built localhost/<name>, nerdctl docker.io/library/<name>.
func asRun(tool, tag string) string {
	switch tool {
	case "podman":
		return "localhost/" + tag
	case "nerdctl":
		return "docker.io/library/" + tag
	}
	return tag
}

// usedBy is the answer of "ps -a --format {{.Image}}": the images the
// containers of the engine of the tool run, one a line.
func usedBy(tool string, images ...string) rule {
	return rule{Tool: tool, Contains: []string{"ps", "-a", "--format", "{{.Image}}"}, Stdout: strings.Join(images, "\n") + "\n"}
}

// imageRemovals is the tags the run removed with "image rm" on the engine
// of the tool, but the build's own tag.
func imageRemovals(res result, tool string) []string {
	var out []string
	for _, c := range callsOf(res, tool, "image", "rm") {
		s := sub(c)
		if tag := s[len(s)-1]; !strings.HasPrefix(tag, "cactusfs:deploy-") {
			out = append(out, tag)
		}
	}
	return out
}

// tagEngines are the Docker targets an image is built on: Docker Desktop,
// Docker CE here, Podman and Rancher Desktop on containerd.
var tagEngines = []dockerEngine{dockerEngines[0], dockerEngines[1], dockerEngines[2], rancherEngines[1]}

// engineOf is the rules of an engine holding s: for Rancher Desktop on
// containerd, with no moby engine answering.
func engineOf(t *testing.T, s engineState) []rule {
	t.Helper()
	if s.e.target == "rancher" {
		return rancherRules(t, s)
	}
	return s.rules(t)
}

// TestImageReproducible: an image made of binaries is given the time of
// their commit - buildx and nerdctl as the build argument SOURCE_DATE_EPOCH
// (buildx without its provenance attestation, which records the build's
// own times; nerdctl with BuildKit's exporter of type docker told to
// rewrite the files' times), Podman as --timestamp - and without it when
// git does not find the commit, which is said. What is said is what holds:
// the same binaries make the same image on Podman and nerdctl, the same
// folder of them on buildx, whose files keep their own times.
func TestImageReproducible(t *testing.T) {
	const (
		sameBinaries = "made with the time of the commit " + imgCommit + ", " + commitTime + " (SOURCE_DATE_EPOCH), its files' times included: the same binaries make the same image"
		sameFolder   = "made with the time of the commit " + imgCommit + ", " + commitTime + " (SOURCE_DATE_EPOCH): the same folder of binaries makes the same image - Docker's builder keeps the files' own times, so a copy of the folder made anew makes an image of another digest"
		rewrite      = "type=docker,rewrite-timestamp=true"
	)
	cases := []struct {
		name, env, tool, commit string
		known                   bool
		said                    string
		want                    []string
		absent                  []string
	}{
		{"buildx", "DEPLOY_TARGET=dd\n", "docker", imgCommit, true, sameFolder, []string{"SOURCE_DATE_EPOCH=" + commitTime, "--provenance=false"}, []string{"--timestamp", "--output", rewrite}},
		{"buildx, a changed tree's binaries", "DEPLOY_TARGET=dd\n", "docker", imgCommit + "-dirty", true, sameFolder, []string{"SOURCE_DATE_EPOCH=" + commitTime, "--provenance=false"}, nil},
		{"podman", "DEPLOY_TARGET=podman\n", "podman", imgCommit, true, sameBinaries, []string{"--timestamp", commitTime}, []string{"SOURCE_DATE_EPOCH=" + commitTime, "--provenance=false", rewrite}},
		{"nerdctl", "DEPLOY_TARGET=rancher\nRANCHER_ENGINE=containerd\n", "nerdctl", imgCommit, true, sameBinaries, []string{"SOURCE_DATE_EPOCH=" + commitTime, "--output", rewrite}, []string{"--provenance=false", "--timestamp"}},
		{"a commit git does not find", "DEPLOY_TARGET=dd\n", "docker", imgCommit, false, "", nil, []string{"SOURCE_DATE_EPOCH=" + commitTime, "--provenance=false", "--timestamp", rewrite}},
		{"nerdctl, a commit git does not find", "DEPLOY_TARGET=rancher\nRANCHER_ENGINE=containerd\n", "nerdctl", imgCommit, false, "", nil, []string{"SOURCE_DATE_EPOCH=" + commitTime, "--output", rewrite}},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.imageBinaries(c.commit, runtime.GOARCH)
				f := newImgFix(c.commit)
				var rules []rule
				if c.known {
					rules = append(rules, commitTimeRule(imgCommit))
				}
				rules = append(rules, gitRules("")...)
				r.rules(append(rules, madeHere(c.tool, f)...)...)
				res := r.image("KMS=local\n"+c.env, resolveAndDeliver, false, "XDG_RUNTIME_DIR=/run/user/1000")
				res.want(t, 0, "resolved "+f.ref())
				if c.known {
					res.want(t, 0, c.said)
				} else {
					res.want(t, 0, "git does not find the commit '"+c.commit+"'", "the same binaries make an image of another digest")
				}
				b := builds(res)
				if len(b) != 1 {
					t.Fatalf("built %d times: %v", len(b), b)
				}
				args := b[0].Args
				if c.tool != "podman" && len(c.want) > 0 && !containsRun(args, []string{"--build-arg", "SOURCE_DATE_EPOCH=" + commitTime}) {
					t.Errorf("the build is not given SOURCE_DATE_EPOCH as a build argument: %q", args)
				}
				if c.tool == "podman" && !containsRun(args, []string{"--timestamp", commitTime}) {
					t.Errorf("podman's build is not given --timestamp %s: %q", commitTime, args)
				}
				if c.tool == "nerdctl" && c.known && !containsRun(args, []string{"--output", rewrite}) {
					t.Errorf("nerdctl's build is not given --output %s: %q", rewrite, args)
				}
				for _, w := range c.want {
					if !slices.Contains(args, w) {
						t.Errorf("the build lacks %q: %q", w, args)
					}
				}
				for _, w := range c.absent {
					if slices.Contains(args, w) {
						t.Errorf("the build holds %q: %q", w, args)
					}
				}
			})
		}
		// An image from source, or given, is not given a time: the same
		// binaries are what is reproducible.
		t.Run("from source", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			f := newImgFix(imgCommit)
			r.rules(append(append([]rule{commitTimeRule(imgCommit)}, gitRules("")...), madeHere("docker", f)...)...)
			res := r.image("KMS=local\nDEPLOY_TARGET=dd\nIMAGE_SOURCE=source\n", resolveAndDeliver, false)
			res.want(t, 0, "resolved "+f.ref())
			for _, b := range builds(res) {
				if slices.Contains(b.Args, "SOURCE_DATE_EPOCH="+commitTime) || slices.Contains(b.Args, "--provenance=false") {
					t.Errorf("a build from source is given the commit's time: %q", b.Args)
				}
			}
			if len(callsOf(res, "git", "show")) != 0 {
				t.Error("git was asked for a time a build from source does not take")
			}
		})
	})
}

// TestImageTagsRecorded: the tag an install builds on the target's engine,
// which the engine lacked, is recorded in the entry's file "images"
// (0600) as "target <tag> <engine ID>"; one the engine held before - another
// deployment's, or the operator's - and an IMAGE given are not. Each line
// names the engine's ID recorded for the deployment.
func TestImageTagsRecorded(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range tagEngines {
			for _, before := range []bool{false, true} {
				name := e.target + "/" + e.tool + ", made"
				if before {
					name = e.target + "/" + e.tool + ", there before"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					r := secretsRig(t, r.fresh(t))
					f := newImgFix(imgCommit)
					rules := madeHere(e.tool, f)
					if !before {
						rules = append([]rule{tagAbsent(e.tool, f)}, rules...)
					}
					r.rules(append(rules, engineOf(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)...)
					res := dockerRun(r, e, "KMS=local\n", "--install")
					res.want(t, 0, "ready: cactusfs runs "+f.ref())
					images := filepath.Join(r.stateOf(e.targetID), "images")
					if before {
						if _, err := os.Stat(images); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("a tag the engine held before was recorded: %v", err)
						}
						return
					}
					res.want(t, 0, "image: recorded "+f.ref()+" (on "+engineIdentityOf(e)+", engine "+e.id+") in "+images)
					if got := read(t, images); got != "target "+f.ref()+" "+e.id+"\n" {
						t.Errorf("the entry records %q", got)
					}
					if info, err := os.Stat(images); err != nil || info.Mode().Perm() != 0o600 {
						t.Errorf("the record's mode: %v %v", info, err)
					}
					// Asked before the tag is made, never after.
					ask := callIndex(res, e.tool, "--format", "{{.Id}}", f.ref())
					tag := callIndex(res, e.tool, "image", "tag")
					if ask < 0 || tag < 0 || ask > tag {
						t.Errorf("the engine was asked for the tag at %d, and it was made at %d", ask, tag)
					}
				})
			}
			if e.tool == "nerdctl" {
				// nerdctl takes a given image by its RepoDigests, with
				// answers of its own (TestRancherGivenImage).
				continue
			}
			t.Run(e.target+"/"+e.tool+", an image given", func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(engineOf(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)
				res := dockerRun(r, e, imageEnv, "--install")
				res.want(t, 0, "ready: cactusfs runs "+nodeImage)
				if _, err := os.Stat(filepath.Join(r.stateOf(e.targetID), "images")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("an image given was recorded: %v", err)
				}
			})
		}
	})
}

// engineIdentityOf is the engine an install names and records.
func engineIdentityOf(e dockerEngine) string {
	switch {
	case e.target == "rancher":
		return rancherIdentity(e.tool)
	case e.tool == "podman":
		return "podman (local)"
	}
	return "docker --context default"
}

// installedFrom is a rig on which an install of the binaries made the
// image f1 and recorded its tag: the base of the upgrade cases.
func installedFrom(t *testing.T, r *rig, e dockerEngine, f1 imgFix) *rig {
	t.Helper()
	base := secretsRig(t, r.fresh(t))
	base.rules(append(append([]rule{tagAbsent(e.tool, f1)}, madeHere(e.tool, f1)...), engineOf(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)...)
	dockerRun(base, e, "KMS=local\n", "--install").want(t, 0, "ready: cactusfs runs "+f1.ref())
	if got := read(t, filepath.Join(base.stateOf(e.targetID), "images")); got != "target "+f1.ref()+" "+e.id+"\n" {
		t.Fatalf("the first install recorded %q", got)
	}
	return base
}

// TestImageTagsUpgrade: once the new node is ready and <name>-previous is
// removed, the tag the replaced node ran is removed from the engine - when
// the script recorded making it and no container of the engine, stopped
// ones included, names it - and its line taken out of the record; one a
// container names is kept, one the script did not record (an IMAGE given)
// is never removed, and a removal the engine refuses is said and keeps its
// line. A node that does not become ready removes nothing.
func TestImageTagsUpgrade(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range tagEngines {
			f1 := newImgFix(imgCommit)
			base := installedFrom(t, r, e, f1)
			state := base.stateOf(e.targetID)
			cases := []struct {
				name    string
				env     string
				extra   []rule
				ready   bool
				code    int
				removed bool
				want    []string
			}{
				{name: "removed", ready: true, removed: true, want: []string{"image: removed " + f1.ref() + " on " + engineIdentityOf(e) + ", which the script had made"}},
				{
					name: "another container runs it", ready: true,
					extra: []rule{usedBy(e.tool, "registry.example.test/web:1", asRun(e.tool, f1.ref()))},
					want:  []string{"image: " + f1.ref() + " is kept: a container on " + engineIdentityOf(e) + " still runs it or was made from it"},
				},
				{
					name: "the engine refuses", ready: true,
					extra: []rule{{Tool: e.tool, Contains: []string{"image", "rm", f1.ref()}, Stderr: "conflict: unable to remove repository reference\n", Exit: 1}},
					want:  []string{"removing the image " + f1.ref() + " on " + engineIdentityOf(e) + " failed (the engine's message is above): it is left, and stays in"},
				},
				{name: "not ready", env: "WAIT_TIMEOUT=3\n", code: 1, want: []string{"the node is DOWN"}},
			}
			for _, c := range cases {
				t.Run(e.target+"/"+e.tool+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					f2 := newImgFix(imgCommit)
					s := installedState(t, e, state, asRun(e.tool, f1.ref()))
					s.inUse = []string{"http://127.0.0.1:9000/", "http://127.0.0.1:9001/"}
					s.ready = c.ready
					rules := append(slices.Clone(c.extra), tagAbsent(e.tool, f2))
					rules = append(rules, madeHere(e.tool, f2)...)
					r.rules(append(rules, engineOf(t, s)...)...)
					res := dockerRun(r, e, "KMS=local\n"+c.env, "--install")
					want := c.want
					if c.name == "not ready" && e.tool == "nerdctl" {
						// On nerdctl the old node was removed, and a new one
						// that ran is left running (TestRancherUpgrade).
						want = []string{"it is left running, as it may have changed the metadata already"}
					}
					res.want(t, c.code, want...)
					removals := imageRemovals(res, e.tool)
					record := read(t, filepath.Join(r.stateOf(e.targetID), "images"))
					switch {
					case c.removed:
						if !slices.Equal(removals, []string{f1.ref()}) {
							t.Errorf("removed %q, want %s", removals, f1.ref())
						}
						if record != "target "+f2.ref()+" "+e.id+"\n" {
							t.Errorf("the record is %q", record)
						}
						rm := callIndex(res, e.tool, "image", "rm", f1.ref())
						// After the old node is gone, never before - on nerdctl
						// removed before the new node ran (its ports are held
						// until it is), elsewhere kept as cactusfs-previous until
						// the new one was ready.
						prev := callIndex(res, e.tool, "rm", "cactusfs-previous")
						if e.tool == "nerdctl" {
							prev = slices.IndexFunc(res.calls, func(c call) bool { return c.Tool == "nerdctl" && slices.Equal(sub(c), []string{"rm", "cactusfs"}) })
							if ready := slices.IndexFunc(res.calls, func(c call) bool { return c.Tool == "curl" && !slices.Contains(c.Args, "%{num_connects}") }); ready < 0 || rm < ready {
								t.Errorf("the image removed at %d, before the new node was ready (%d)", rm, ready)
							}
						}
						ps := callIndex(res, e.tool, "ps", "-a", "--format", "{{.Image}}")
						if prev < 0 || ps < prev || rm < ps {
							t.Errorf("cactusfs-previous removed at %d, the containers listed at %d, the image removed at %d", prev, ps, rm)
						}
					case c.code == 0 && len(c.extra) > 0 && c.extra[0].Exit != 0:
						if !slices.Equal(removals, []string{f1.ref()}) {
							t.Errorf("tried %q, want %s", removals, f1.ref())
						}
						if record != "target "+f1.ref()+" "+e.id+"\ntarget "+f2.ref()+" "+e.id+"\n" {
							t.Errorf("the record is %q", record)
						}
					default:
						if len(removals) != 0 {
							t.Errorf("removed %q", removals)
						}
						if record != "target "+f1.ref()+" "+e.id+"\ntarget "+f2.ref()+" "+e.id+"\n" {
							t.Errorf("the record is %q", record)
						}
					}
				})
			}
		}
		// A node that ran an IMAGE given: the script did not make it, and
		// never removes it.
		for _, e := range []dockerEngine{dockerEngines[0], dockerEngines[2]} {
			t.Run(e.target+" an image given before", func(t *testing.T) {
				t.Parallel()
				base := dockerInstalled(t, r, e, imageEnv)
				r := clone(t, base)
				f2 := newImgFix(imgCommit)
				s := installedState(t, e, base.stateOf(e.targetID), nodeImage)
				s.inUse = []string{"http://127.0.0.1:9000/", "http://127.0.0.1:9001/"}
				r.rules(append(append([]rule{tagAbsent(e.tool, f2)}, madeHere(e.tool, f2)...), s.rules(t)...)...)
				res := dockerRun(r, e, "KMS=local\n", "--install")
				res.want(t, 0, "ready: cactusfs runs "+f2.ref(), "image: "+nodeImage+", which the replaced node ran, is not a tag the script recorded making: it is left")
				if got := imageRemovals(res, e.tool); len(got) != 0 {
					t.Errorf("removed %q", got)
				}
			})
		}
	})
}

// TestImageTagsPurge: a purge removes, after the secrets' volumes and
// before the entry, every tag the entry records that no container of the
// engine names; a tag a container names is kept and said, a line the
// script does not write is left and warned of, and nothing the record does
// not name is removed. A dry run removes nothing. Uninstall removes none.
func TestImageTagsPurge(t *testing.T) {
	t.Parallel()
	const (
		unused = "cactusfs:0.7.0-411-0123456789ab"
		used   = "cactusfs:0.7.0-412-feedfacefeed"
		other  = "cactusfs:0.7.0-410-abcdefabcdef"
	)
	confirm := []string{"--uninstall", "--purge", "--purge-confirm=cactusfs"}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range tagEngines {
			var base *rig
			if e.target == "rancher" {
				// Rancher Desktop on containerd, installed from binaries
				// with its own answers (no moby engine answers there).
				base = secretsRig(t, r.fresh(t))
				base.rules(rancherRules(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)
				dockerRun(base, e, "KMS=local\n", "--install").want(t, 0, "ready: cactusfs runs")
			} else {
				base = dockerInstalled(t, r, e, imageEnv)
			}
			state := base.stateOf(e.targetID)
			for _, dry := range []bool{false, true} {
				name := e.target + "/" + e.tool
				if dry {
					name += " dry run"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					put(t, filepath.Join(r.stateOf(e.targetID), "images"), "target "+unused+" "+e.id+"\ntarget "+used+" "+e.id+"\nsomething else\n")
					s := installedState(t, e, state, nodeImage)
					s.purged = true
					r.rules(append([]rule{usedBy(e.tool, asRun(e.tool, used), "registry.example.test/web:1", asRun(e.tool, other))}, engineOf(t, s)...)...)
					args := slices.Clone(confirm)
					if dry {
						args = append(args, "--dry-run")
					}
					res := dockerRun(r, e, imageEnv, args...)
					if dry {
						res.want(t, 0, "run: "+engineCLI(e)+" image rm "+unused)
						noChange(t, res)
						return
					}
					res.want(t, 0, "image: removed "+unused+" on "+engineIdentityOf(e)+", which the script had made",
						"image: "+used+" is kept: a container on "+engineIdentityOf(e)+" still runs it or was made from it",
						"holds a line the script does not write, which is left: something else", "purged: cactusfs")
					if got := imageRemovals(res, e.tool); !slices.Equal(got, []string{unused}) {
						t.Errorf("removed %q, want %s alone", got, unused)
					}
					keys := callIndex(res, e.tool, "volume", "rm", "cactusfs-config", "cactusfs-secrets")
					rm := callIndex(res, e.tool, "image", "rm", unused)
					if keys < 0 || rm < keys {
						t.Errorf("the secrets' volumes deleted at %d, the image at %d", keys, rm)
					}
					if lineAt(res, "image: removed "+unused) > lineAt(res, "purge: deleted "+r.stateOf(e.targetID)) {
						t.Error("the image was removed after the entry")
					}
					if _, err := os.Stat(r.stateOf(e.targetID)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("the entry is still there: %v", err)
					}
				})
			}
			t.Run(e.target+"/"+e.tool+" uninstall", func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				put(t, filepath.Join(r.stateOf(e.targetID), "images"), "target "+unused+" "+e.id+"\n")
				r.rules(engineOf(t, installedState(t, e, state, nodeImage))...)
				res := dockerRun(r, e, imageEnv, "--uninstall")
				res.want(t, 0, "removed the container cactusfs")
				if got := imageRemovals(res, e.tool); len(got) != 0 {
					t.Errorf("an uninstall removed %q", got)
				}
				if read(t, filepath.Join(r.stateOf(e.targetID), "images")) != "target "+unused+" "+e.id+"\n" {
					t.Error("an uninstall changed the record")
				}
			})
		}
	})
}

// TestImageTagsComposeUpgrade: once every node of a compose cluster is
// ready on the new image, the tag the nodes ran before is removed when the
// script recorded making it and no container names it - the init
// container, which an upgrade does not make again, keeps the image it was
// made from; a tag no node ran is not touched.
func TestImageTagsComposeUpgrade(t *testing.T) {
	t.Parallel()
	const (
		old   = "cactusfs:0.7.0-411-0123456789ab"
		first = "cactusfs:0.7.0-410-abcdefabcdef"
	)
	newEnv := strings.Replace(clusterEnv, nodeImage, newImage, 1)
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range []dockerEngine{dockerEngines[0], dockerEngines[2]} {
			base := clusterInstalled(t, r, e, clusterEnv)
			for _, c := range []struct {
				name    string
				initRun string
				removed bool
			}{
				{"removed", first, true},
				{"the init container runs it", old, false},
			} {
				t.Run(e.target+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					state := r.stateOf(e.targetID)
					put(t, filepath.Join(state, "images"), "target "+old+" "+e.id+"\ntarget "+first+" "+e.id+"\n")
					s := runningCluster(t, e, state, asRun(e.tool, old))
					s.ctrs[0] = "cactusfs-init-1|" + e.target + "|init|exited|" + asRun(e.tool, c.initRun)
					var images []string
					for _, ctr := range s.ctrs {
						images = append(images, ctr[strings.LastIndex(ctr, "|")+1:])
					}
					r.rules(append([]rule{usedBy(e.tool, images[0])}, s.rules(t)...)...)
					res := clusterRun(r, e, newEnv+"WAIT_TIMEOUT=1\n", "--install")
					res.want(t, 0, "ready: the cluster cactusfs runs "+newImage)
					removals := imageRemovals(res, e.tool)
					record := read(t, filepath.Join(state, "images"))
					if c.removed {
						if !slices.Equal(removals, []string{old}) || record != "target "+first+" "+e.id+"\n" {
							t.Errorf("removed %q, the record %q", removals, record)
						}
						// After the last node is ready.
						up := slices.IndexFunc(res.calls, func(c call) bool { return slices.Contains(c.Args, "--no-deps") && slices.Contains(c.Args, "node2") })
						if rm := callIndex(res, e.tool, "image", "rm", old); up < 0 || rm < up {
							t.Errorf("the last node made again at %d, the image removed at %d", up, rm)
						}
						return
					}
					res.want(t, 0, "image: "+old+" is kept: a container on ")
					if len(removals) != 0 || record != "target "+old+" "+e.id+"\ntarget "+first+" "+e.id+"\n" {
						t.Errorf("removed %q, the record %q", removals, record)
					}
				})
			}
		}
	})
}

// engineCLI is the command an engine's call is printed with.
func engineCLI(e dockerEngine) string {
	switch e.tool {
	case "nerdctl":
		return "nerdctl --namespace default"
	case "podman":
		return "podman"
	}
	return "docker --context default"
}

// hereEngine is the ID of the Docker engine this machine's context default
// reaches, in the cases of CE_HOST (the server's is engineID).
const hereEngine = "HERE-ENGINE"

// hereInfo is the answer of this machine's engine, through the context
// given, of its ID: a rule before the server's (DOCKER_HOST=ssh://, no
// context), which every other "info" call meets.
func hereInfo(context, id string) rule {
	return rule{Tool: "docker", Contains: []string{"--context", context, "info", "--format", "{{.ID}}"}, Stdout: id + "\n"}
}

// TestImageTagsOnCEHost: for Docker CE on a server, the image built here
// and loaded there is recorded twice - "here:<context> <tag> <ID>" for the
// copy of this machine's engine, with the ID that engine says, "target
// <tag> <ID>" for the server's, with the deployment's engine ID, when each
// lacked it - and removed from each by its own route: this machine's
// Docker CLI with the context, the server's over DOCKER_HOST=ssh://. After
// an upgrade the replaced tag goes from both and this machine's copy of
// the run stays; a copy recorded through another context than the
// settings' is left. A line whose engine is not the one reached now - the
// context pointed at another engine, the server's engine another, or no ID
// at all (an earlier build of the script) - is named and left, in the
// record.
func TestImageTagsOnCEHost(t *testing.T) {
	const ce = "KMS=local\nDEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n"
	const server = "ssh://deploy@server.example.test"
	const (
		old  = "cactusfs:0.7.0-411-0123456789ab"
		now  = "cactusfs:0.7.0-412-feedfacefeed"
		away = "cactusfs:0.7.0-410-abcdefabcdef"
	)
	// removedFrom is where each "image rm" of the run went, and what.
	removedFrom := func(res result) []string {
		var got []string
		for _, c := range callsOf(res, "docker", "image", "rm") {
			where := "here"
			if c.Env["DOCKER_HOST"] == server {
				where = "server"
			} else if !containsRun(c.Args, []string{"--context", "default"}) {
				where = "elsewhere"
			}
			got = append(got, where+" "+c.Args[len(c.Args)-1])
		}
		return got
	}
	// upgradeThenPurge is the code of a run that records the lines given,
	// asks the server for its ID as an install does, then removes what an
	// upgrade from old to now removes, then what a purge removes.
	upgradeThenPurge := func(lines string) string {
		return `host_key_check deploy@server.example.test; secrets_settings; engine_id_ask; STATE=$TMPDIR/entry; mkdir "$STATE"; ` +
			`printf '%s' '` + lines + `' >"$STATE/images"; ` +
			`_dk_image=` + now + `; image_tags_replaced ` + old + `; echo "after the upgrade:"; cat "$STATE/images"; ` +
			`image_tags_purge; echo "after the purge:"; cat "$STATE/images" 2>/dev/null; rm -rf "$STATE"`
	}
	eachShell(t, func(t *testing.T, r *rig) {
		t.Run("recorded", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			f := newImgFix(imgCommit)
			r.imageBinaries(imgCommit, "amd64")
			rules := []rule{
				{Tool: "docker", Contains: []string{"--context", "image", "inspect", "--format", "{{.Id}}", f.ref()}, Until: []string{"tag", f.ref()}, Stderr: noSuchImage("docker", f.ref()), Exit: 1},
				{Tool: "docker", Contains: []string{"image", "inspect", "--format", "{{.Id}}", f.ref()}, Until: []string{"load"}, Stderr: noSuchImage("docker", f.ref()), Exit: 1},
				hereInfo("default", hereEngine),
				{Tool: "docker", Contains: []string{"info", "--format", "{{.ID}}"}, Stdout: engineID + "\n"},
			}
			rules = append(rules, gitRules("")...)
			r.rules(append(rules, imgEngine("docker", "amd64", "amd64", f)...)...)
			res := r.image(ce, resolveAndDeliver+`; secrets_settings; engine_id_ask; STATE=$TMPDIR/entry; mkdir "$STATE"; image_tags_record; cat "$STATE/images"; rm -rf "$STATE"`, false)
			res.want(t, 0, "resolved "+f.ref(), "here:default "+f.ref()+" "+hereEngine+"\ntarget "+f.ref()+" "+engineID+"\n",
				"image: recorded "+f.ref()+" (on the Docker engine here, context default, engine "+hereEngine+")",
				"image: recorded "+f.ref()+" (on docker over "+server+", engine "+engineID+")")
			there := callsOf(res, "docker", "--format", "{{.Id}}", f.ref())
			if len(there) != 2 || there[0].Env["DOCKER_HOST"] != "" || there[1].Env["DOCKER_HOST"] != server {
				t.Errorf("the tag was asked of %v", there)
			}
			// This machine's engine is asked through the context, never
			// over the server's route.
			here := callsOf(res, "docker", "--context", "default", "info", "{{.ID}}")
			if len(here) != 1 || here[0].Env["DOCKER_HOST"] != "" {
				t.Errorf("this machine's engine was asked its ID by %v", here)
			}
		})
		t.Run("a copy here whose engine says no ID is not recorded", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			r.rules(rule{Tool: "docker", Contains: []string{"--context", "default", "info"}, Stderr: "Cannot connect to the Docker daemon\n", Exit: 1},
				rule{Tool: "docker", Contains: []string{"info", "--format", "{{.ID}}"}, Stdout: engineID + "\n"})
			code := `host_key_check deploy@server.example.test; secrets_settings; engine_id_ask; STATE=$TMPDIR/entry; mkdir "$STATE"; ` +
				`_img_made=; image_made_note here:default ` + now + `; image_made_note target ` + now + `; image_tags_record; cat "$STATE/images"; rm -rf "$STATE"`
			res := r.image(ce, code, false)
			res.want(t, 0, "the Docker engine here (context default) did not say its ID: the image "+now+" is not recorded, and an upgrade or a purge leaves it there",
				"target "+now+" "+engineID+"\n")
			if strings.Contains(res.stdout, "here:default") {
				t.Errorf("a line without its engine's ID was recorded:\n%s", res.stdout)
			}
		})
		t.Run("removed", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			r.rules(hereInfo("default", hereEngine), hereInfo("other", hereEngine),
				rule{Tool: "docker", Contains: []string{"info", "--format", "{{.ID}}"}, Stdout: engineID + "\n"})
			res := r.image(ce, upgradeThenPurge(
				"here:default "+old+" "+hereEngine+"\ntarget "+old+" "+engineID+"\nhere:default "+now+" "+hereEngine+"\ntarget "+now+" "+engineID+"\nhere:other "+away+" "+hereEngine+"\n"), false)
			res.want(t, 0, "after the upgrade:\nhere:default "+now+" "+hereEngine+"\ntarget "+now+" "+engineID+"\nhere:other "+away+" "+hereEngine+"\n",
				"after the purge:\nhere:other "+away+" "+hereEngine+"\n",
				"purge: the image "+away+" is on the Docker engine here through the context other, which the settings no longer name: it is left")
			want := []string{"server " + old, "here " + old, "here " + now, "server " + now}
			if got := removedFrom(res); !slices.Equal(got, want) {
				t.Errorf("removed %q, want %q", got, want)
			}
		})
		t.Run("another engine", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			// The context default now reaches another engine than the one
			// the copies here were made on, and the server's engine is not
			// the one its line names: nothing is removed from either, and
			// every line stays.
			r.rules(hereInfo("default", "ANOTHER-HERE"),
				rule{Tool: "docker", Contains: []string{"info", "--format", "{{.ID}}"}, Stdout: engineID + "\n"})
			lines := "here:default " + old + " " + hereEngine + "\ntarget " + old + " ENGINE-GONE\nhere:default " + now + " " + hereEngine + "\ntarget " + now + " ENGINE-GONE\n"
			res := r.image(ce, upgradeThenPurge(lines), false)
			res.want(t, 0, "after the upgrade:\n"+lines, "after the purge:\n"+lines,
				"image: "+old+" was recorded on the engine 'ENGINE-GONE', and the engine reached now (on docker over "+server+") is '"+engineID+"': it is left, and stays in",
				"image: "+old+" was recorded on the engine '"+hereEngine+"', and the engine reached now (on the Docker engine here, context default) is 'ANOTHER-HERE': it is left")
			if got := removedFrom(res); len(got) != 0 {
				t.Errorf("removed %q from an engine the record does not name", got)
			}
			if len(callsOf(res, "docker", "ps")) != 0 {
				t.Error("the containers of an engine the record does not name were listed")
			}
			// Asked once a run, through the context.
			if got := len(callsOf(res, "docker", "--context", "default", "info", "{{.ID}}")); got != 1 {
				t.Errorf("this machine's engine was asked its ID %d times", got)
			}
		})
		t.Run("lines without an ID", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			r.rules(hereInfo("default", hereEngine),
				rule{Tool: "docker", Contains: []string{"info", "--format", "{{.ID}}"}, Stdout: engineID + "\n"})
			lines := "here:default " + old + "\ntarget " + old + "\ntarget " + now + "\n"
			res := r.image(ce, upgradeThenPurge(lines), false)
			res.want(t, 0, "after the upgrade:\n"+lines, "after the purge:\n"+lines,
				"image: "+old+" (on docker over "+server+") was recorded without the ID of its engine, by an earlier build of the script, so which engine holds it cannot be checked: it is left",
				"image: "+old+" (on the Docker engine here, context default) was recorded without the ID of its engine")
			if got := removedFrom(res); len(got) != 0 {
				t.Errorf("removed %q by lines without an engine", got)
			}
		})
	})
}

// TestTLSHostsWarning: an install whose S3 is published beyond the
// loopback - a Docker target's BIND, a compose cluster, a NodePort or
// LoadBalancer - with TLS_HOSTS empty and no certificate of the operator's
// warns that the development certificate holds only localhost and the
// loopback addresses, and that names cannot be added later; it does not
// refuse. Published on the loopback, or with TLS_HOSTS, it says nothing.
func TestTLSHostsWarning(t *testing.T) {
	t.Parallel()
	const warned = "TLS_HOSTS is empty: the development certificate holds only localhost and the loopback addresses"
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name, env string
			warn      bool
			where     string
		}{
			{"every address", "BIND=0.0.0.0\n", true, "S3 is published on 0.0.0.0:9000 (BIND)"},
			{"an address of the machine", "BIND=192.0.2.10\n", true, "S3 is published on 192.0.2.10:9000 (BIND)"},
			{"every address, with TLS_HOSTS", "BIND=0.0.0.0\nTLS_HOSTS=s3.example.test\n", false, ""},
			{"every address, with a certificate of one's own", "BIND=0.0.0.0\nTLS_CERT_FILE=~/own.crt\nTLS_KEY_FILE=~/own.key\n", false, ""},
			{"the loopback", "", false, ""},
			{"another loopback address", "BIND=127.0.0.2\n", false, ""},
		} {
			t.Run("docker "+c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				put(t, filepath.Join(r.home, "own.crt"), "fixture-own-crt\n")
				put(t, filepath.Join(r.home, "own.key"), "fixture-own-key\n")
				e := dockerEngines[0]
				s := engineState{e: e, devInit: devInitOutput(), ready: true}
				r.rules(s.rules(t)...)
				res := dockerRun(r, e, imageEnv+c.env, "--install")
				res.want(t, 0, "ready: cactusfs runs")
				if got := strings.Contains(res.stderr, warned); got != c.warn {
					t.Errorf("warned: %v, want %v:\n%s", got, c.warn, res.stderr)
				}
				if c.warn {
					res.want(t, 0, "deploy: warning: "+c.where, "names cannot be added to it later", "set TLS_HOSTS to the names and addresses clients use before its first install")
				}
			})
		}
		for _, c := range []struct {
			name, env string
			warn      bool
		}{
			{"compose cluster", "", true},
			{"compose cluster, with TLS_HOSTS", "TLS_HOSTS=s3.example.test\n", false},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				e := dockerEngines[0]
				r.rules(clusterState{e: e, devInit: devInitOutput()}.rules(t)...)
				res := clusterRun(r, e, clusterEnv+c.env, "--install")
				res.want(t, 0, "ready: the cluster cactusfs runs")
				if got := strings.Contains(res.stderr, warned); got != c.warn {
					t.Errorf("warned: %v, want %v:\n%s", got, c.warn, res.stderr)
				}
				if c.warn {
					res.want(t, 0, "deploy: warning: S3 is published on every address of the machine of its engine, at the ports 9000, 9010, 9020 and 9030 (a compose cluster) and "+warned)
				}
			})
		}
		for _, c := range []struct {
			name, env string
			warn      bool
		}{
			{"a NodePort", "", true},
			{"a LoadBalancer", "K8S_SERVICE_TYPE=LoadBalancer\n", true},
			{"a ClusterIP", "K8S_SERVICE_TYPE=ClusterIP\n", false},
			{"a NodePort, with TLS_HOSTS", "TLS_HOSTS=s3.example.test\n", false},
		} {
			t.Run("kubernetes "+c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				f := newImgFix(imgCommit)
				k := kube{mode: "standalone", f: f, image: registry + "@sha256:" + f.hex, devInit: k8sDevInit("standalone")}
				r.rules(k.rules(t)...)
				res := r.kubeRun(kubeEnv(f)+c.env, "--install")
				res.want(t, 0, "ready: cactusfs-node-0 runs")
				if got := strings.Contains(res.stderr, warned); got != c.warn {
					t.Errorf("warned: %v, want %v:\n%s", got, c.warn, res.stderr)
				}
				if c.warn {
					res.want(t, 0, "S3 is published outside the cluster, by a Service of K8S_SERVICE_TYPE=")
				}
			})
		}
	})
}

// TestRancherKubernetesImagesInUse: on Rancher Desktop's Kubernetes, a
// recorded tag is removed - at a purge, or after an upgrade replaced the
// pods that ran it - only when, besides no container of the engine, no pod
// and no workload's template of any namespace names it: by its name, or by
// the digest of its image (a pod's imageID, as the kubelet reports it) -
// another namespace may run the same image under another name, unrecorded.
// When the pods or the workloads cannot be listed every tag is kept; an
// image that cannot be inspected is kept.
func TestRancherKubernetesImagesInUse(t *testing.T) {
	t.Parallel()
	const (
		unused   = "cactusfs:0.7.0-411-0123456789ab"
		byName   = "cactusfs:0.7.0-412-feedfacefeed"
		byTmpl   = "cactusfs:0.7.0-410-abcdefabcdef"
		byDigest = "cactusfs:0.7.0-409-0000beefcafe"
		noInfo   = "cactusfs:0.7.0-408-cafecafecafe"
	)
	manifest := randomHex()
	pods := kjsonNoT(kubeList(
		obj{"kind": "Pod", "metadata": obj{"name": "web-0", "namespace": "shop"}, "spec": obj{"containers": []any{obj{"name": "c", "image": byName}}}},
		obj{
			"kind": "Pod", "metadata": obj{"name": "copy-0", "namespace": "other"},
			"spec":   obj{"containers": []any{obj{"name": "c", "image": "registry.example.test/copy:1"}}},
			"status": obj{"containerStatuses": []any{obj{"name": "c", "image": "registry.example.test/copy:1", "imageID": "docker.io/library/cactusfs@sha256:" + manifest}}},
		},
	))
	workloads := kjsonNoT(kubeList(obj{
		"kind": "Deployment", "metadata": obj{"name": "later", "namespace": "shop"},
		"spec": obj{"template": obj{"spec": obj{"containers": []any{obj{"name": "c", "image": byTmpl}}}}},
	}))
	listed := []rule{
		{Tool: "kubectl", Contains: []string{"get", "pods", "-A"}, Stdout: pods},
		{Tool: "kubectl", Contains: []string{"get", "statefulsets,deployments,daemonsets,jobs,cronjobs", "-A"}, Stdout: workloads},
	}
	inspect := func(tool string) []rule {
		return []rule{
			{Tool: tool, Contains: []string{"image", "inspect", "--format", "{{.Id}} {{.RepoDigests}}", byDigest}, Stdout: "sha256:" + randomHex() + " [docker.io/library/cactusfs@sha256:" + manifest + "]\n"},
			{Tool: tool, Contains: []string{"image", "inspect", "--format", "{{.Id}} {{.RepoDigests}}", noInfo}, Stderr: "no such image\n", Exit: 1},
			{Tool: tool, Contains: []string{"image", "inspect", "--format", "{{.Id}} {{.RepoDigests}}"}, Stdout: "sha256:" + randomHex() + " []\n"},
		}
	}
	all := strings.Join([]string{unused, byName, byTmpl, byDigest, noInfo}, " ")
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct{ engine, tool string }{{"containerd", "nerdctl"}, {"moby", "docker"}} {
			env := "DEPLOY_TARGET=rancher\nRANCHER_KUBERNETES=yes\nRANCHER_ENGINE=" + c.engine + "\nKMS=local\n"
			id := rancherKubeID(c.tool)
			record := ""
			for _, tag := range strings.Fields(all) {
				record += "target " + tag + " " + id + "\n"
			}
			code := func(call string) string {
				return rancherRecord(record, call)
			}
			engine := rancherKubeEngine(c.tool)
			t.Run(c.engine+" purge", func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.rules(append(append(slices.Clone(listed), engine), inspect(c.tool)...)...)
				res := r.image(env, code("k8s_images_rancher purge"), false)
				res.want(t, 0,
					"image: removed "+unused+" on the engine whose images Rancher Desktop's Kubernetes sees, which the script had made",
					"image: "+byName+" is kept: a pod or a workload's template of the cluster names it",
					"image: "+byTmpl+" is kept: a pod or a workload's template of the cluster names it",
					"image: "+byDigest+" is kept: a pod or a workload's template of the cluster runs its image, sha256:"+manifest,
					"the image "+noInfo+" on the engine whose images Rancher Desktop's Kubernetes sees could not be inspected",
					"left:\ntarget "+byName+" "+id+"\ntarget "+byTmpl+" "+id+"\ntarget "+byDigest+" "+id+"\ntarget "+noInfo+" "+id+"\n")
				if got := imageRemovals(res, c.tool); !slices.Equal(got, []string{unused}) {
					t.Errorf("removed %q, want %s alone", got, unused)
				}
				// Listed once each, through the context, before any removal.
				for _, kinds := range []string{"pods", "statefulsets,deployments,daemonsets,jobs,cronjobs"} {
					l := callsOf(res, "kubectl", "get", kinds, "-A")
					if len(l) != 1 || !containsRun(l[0].Args, []string{"--context", "rancher-desktop"}) {
						t.Errorf("the %s of every namespace were listed by %v", kinds, l)
					}
					if i, rm := callIndex(res, "kubectl", "get", kinds, "-A"), callIndex(res, c.tool, "image", "rm"); rm < i {
						t.Errorf("the %s listed at %d, an image removed at %d", kinds, i, rm)
					}
				}
			})
			t.Run(c.engine+" after an upgrade", func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.rules(append(append(slices.Clone(listed), engine), inspect(c.tool)...)...)
				res := r.image(env, code("RESOLVED_IMAGE=cactusfs:0.7.0-413-111111111111; k8s_images_rancher replaced "+unused+" "+byName), false)
				res.want(t, 0, "image: removed "+unused+" on the engine", "image: "+byName+" is kept: a pod or a workload's template of the cluster names it")
				if got := imageRemovals(res, c.tool); !slices.Equal(got, []string{unused}) {
					t.Errorf("removed %q, want %s alone", got, unused)
				}
			})
			for _, failing := range []string{"pods", "statefulsets,deployments,daemonsets,jobs,cronjobs"} {
				t.Run(c.engine+" the "+failing+" cannot be listed", func(t *testing.T) {
					t.Parallel()
					r := r.fresh(t)
					r.rules(append([]rule{{Tool: "kubectl", Contains: []string{"get", failing, "-A"}, Stderr: "Error from server (Forbidden)\n", Exit: 1}, engine}, listed...)...)
					res := r.image(env, code("k8s_images_rancher purge"), false)
					res.want(t, 0, "the "+failing+" of every namespace could not be listed (kubectl's message is above): the images in", "left:\n"+record)
					if got := imageRemovals(res, c.tool); len(got) != 0 {
						t.Errorf("removed %q though the cluster's use of them is not known", got)
					}
				})
			}
		}
	})
}

// rancherRecord is shell code that runs call on Rancher Desktop's
// Kubernetes over an entry whose record of the image tags is record, with
// the cluster's UID as the deployment's engine ID, and then prints what is
// left of the record after "left:".
func rancherRecord(record, call string) string {
	return `rancher_resolve; STATE=$TMPDIR/entry; mkdir "$STATE"; printf '%s' '` + record + `' >"$STATE/images"; _engine_id=` + clusterUID + `; ` +
		call + `; echo "left:"; cat "$STATE/images" 2>/dev/null; rm -rf "$STATE"`
}

// TestRancherKubernetesRuntimeSwitched: a tag made for Rancher Desktop's
// Kubernetes is recorded with the cluster's UID and the ID of the engine
// that holds it. When Rancher Desktop's runtime is switched (moby to
// containerd, or back) the cluster and its UID stay, and the engine is
// another: no tag of the record is removed, by a purge or after an upgrade,
// and every line stays - as does a line an earlier build wrote with the
// cluster's UID alone, and every line while the engine does not say its ID.
func TestRancherKubernetesRuntimeSwitched(t *testing.T) {
	t.Parallel()
	const (
		tagA = "cactusfs:0.7.0-411-0123456789ab"
		tagB = "cactusfs:0.7.0-412-feedfacefeed"
	)
	listed := []rule{
		{Tool: "kubectl", Contains: []string{"get", "pods", "-A"}, Stdout: kjsonNoT(kubeList())},
		{Tool: "kubectl", Contains: []string{"get", "statefulsets,deployments,daemonsets,jobs,cronjobs", "-A"}, Stdout: kjsonNoT(kubeList())},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct{ engine, tool, before string }{{"containerd", "nerdctl", "docker"}, {"moby", "docker", "nerdctl"}} {
			env := "DEPLOY_TARGET=rancher\nRANCHER_KUBERNETES=yes\nRANCHER_ENGINE=" + c.engine + "\nKMS=local\n"
			for _, s := range []struct {
				name, record, want string
				rules              []rule
			}{
				{
					name:   "switched",
					record: "target " + tagA + " " + rancherKubeID(c.before) + "\ntarget " + tagB + " " + rancherKubeID(c.before) + "\n",
					rules:  []rule{rancherKubeEngine(c.tool)},
					want:   "image: " + tagA + " was recorded on the engine '" + rancherKubeID(c.before) + "', and the engine reached now (on the engine whose images Rancher Desktop's Kubernetes sees) is '" + rancherKubeID(c.tool) + "': it is left",
				},
				{
					name:   "recorded with the cluster's UID alone",
					record: "target " + tagA + " " + clusterUID + "\ntarget " + tagB + " " + clusterUID + "\n",
					rules:  []rule{rancherKubeEngine(c.tool)},
					want:   "image: " + tagA + " (on the engine whose images Rancher Desktop's Kubernetes sees) was recorded with the ID of the cluster alone, '" + clusterUID + "'",
				},
				{
					name:   "the engine says no ID",
					record: "target " + tagA + " " + rancherKubeID(c.tool) + "\ntarget " + tagB + " " + rancherKubeID(c.tool) + "\n",
					rules:  []rule{{Tool: c.tool, Contains: []string{"info", "--format", "{{.ID}}"}, Stderr: "cannot connect\n", Exit: 1}},
					want:   "the engine whose images Rancher Desktop's Kubernetes sees did not say its ID: the image " + tagA + " recorded on it is left",
				},
			} {
				for _, call := range []struct{ name, code string }{
					{"purge", "k8s_images_rancher purge"},
					{"after an upgrade", "RESOLVED_IMAGE=cactusfs:0.7.0-413-111111111111; k8s_images_rancher replaced " + tagA + " " + tagB},
				} {
					t.Run(c.engine+" "+s.name+" "+call.name, func(t *testing.T) {
						t.Parallel()
						r := r.fresh(t)
						r.rules(append(slices.Clone(s.rules), listed...)...)
						res := r.image(env, rancherRecord(s.record, call.code), false)
						res.want(t, 0, s.want, "left:\n"+s.record)
						if got := imageRemovals(res, c.tool); len(got) != 0 {
							t.Errorf("removed %q from an engine the record does not name", got)
						}
						if got := imageRemovals(res, c.before); len(got) != 0 {
							t.Errorf("removed %q through the engine of before the switch", got)
						}
					})
				}
			}
		}
	})
}

// TestImageRecordDrop: a line taken out of the record keeps the rest, and
// the record goes with its last line (grep's exit 1, no line left, is
// success). A record grep cannot read (its exit 2: here a grep that says so,
// which a test run as root could not get from a file's mode) is said, left
// as it was, and its copy removed - never replaced by an empty record.
func TestImageRecordDrop(t *testing.T) {
	t.Parallel()
	const (
		lineA = "target cactusfs:0.7.0-411-0123456789ab ENGINE-ONE"
		lineB = "here:default cactusfs:0.7.0-412-feedfacefeed HERE-ENGINE"
	)
	unreadable := `grep() { if [ "$1" = -vxF ]; then echo "grep: $3: Permission denied" >&2; return 2; fi; command grep "$@"; }; `
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name, record, drop, prelude, want, left string
		}{
			{name: "one of two", record: lineA + "\n" + lineB + "\n", drop: lineA, want: "dropped", left: lineB + "\n"},
			{name: "the last", record: lineA + "\n", drop: lineA, want: "dropped", left: "(no record)\n"},
			{name: "unreadable", record: lineA + "\n" + lineB + "\n", drop: lineA, prelude: unreadable, want: "kept", left: lineA + "\n" + lineB + "\n"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				code := `STATE=$TMPDIR/entry; mkdir "$STATE"; printf '%s' '` + c.record + `' >"$STATE/images"; ` + c.prelude +
					`if image_record_drop '` + c.drop + `'; then echo dropped; else echo kept; fi; ` +
					`if [ -e "$STATE/images.new" ]; then echo "the copy is left"; fi; ` +
					`echo "left:"; cat "$STATE/images" 2>/dev/null || echo "(no record)"; rm -rf "$STATE"`
				res := r.image("DEPLOY_TARGET=rancher\nRANCHER_KUBERNETES=yes\nRANCHER_ENGINE=moby\nKMS=local\n", code, false)
				res.want(t, 0, c.want+"\n", "left:\n"+c.left)
				if strings.Contains(res.output(), "the copy is left") {
					t.Error("images.new was left")
				}
				warned := strings.Contains(res.output(), "/images could not be read (grep's exit 2, its message is above): the line '"+lineA+"' stays in it")
				if warned != (c.prelude != "") {
					t.Errorf("warned %v:\n%s", warned, res.output())
				}
			})
		}
	})
}

// TestImageTagsEngineReplaced: --engine-replaced takes the recorded engine
// as gone with its images: the lines of the target's engine leave the
// record, each said, and no image is removed from the engine that replaced
// it - nor ever later by those lines; this machine's copies stay recorded.
// The tag the install makes on the new engine is recorded with its ID.
func TestImageTagsEngineReplaced(t *testing.T) {
	t.Parallel()
	const here = "here:default cactusfs:0.7.0-410-abcdefabcdef " + hereEngine
	eachShell(t, func(t *testing.T, r *rig) {
		e := dockerEngines[0]
		f1 := newImgFix(imgCommit)
		base := installedFrom(t, r, e, f1)
		images := filepath.Join(base.stateOf(e.targetID), "images")
		put(t, images, read(t, images)+here+"\n")
		for _, dry := range []bool{false, true} {
			name := "install"
			if dry {
				name = "dry run"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				images := filepath.Join(r.stateOf(e.targetID), "images")
				f2 := newImgFix(imgCommit)
				rules := []rule{{Tool: "docker", Contains: []string{"info", "{{.ID}}"}, Stdout: "ENGINE-TWO\n"}, tagAbsent(e.tool, f2)}
				rules = append(rules, madeHere(e.tool, f2)...)
				r.rules(append(rules, engineState{e: e, devInit: devInitOutput(), ready: true}.rules(t)...)...)
				args := []string{"--install", "--engine-replaced"}
				if dry {
					args = append(args, "--dry-run")
				}
				res := dockerRun(r, e, "KMS=local\n", args...)
				if dry {
					res.want(t, 0)
					if got := read(t, images); got != "target "+f1.ref()+" "+engineID+"\n"+here+"\n" {
						t.Errorf("a dry run changed the record: %q", got)
					}
					return
				}
				res.want(t, 0, "ready: cactusfs runs "+f2.ref(),
					"image: --engine-replaced: 'target "+f1.ref()+" "+engineID+"' was recorded on the engine '"+engineID+"', which is taken as gone with its images: the line leaves "+images+", and nothing is removed")
				if got := read(t, images); got != here+"\ntarget "+f2.ref()+" ENGINE-TWO\n" {
					t.Errorf("the record is %q", got)
				}
				if got := imageRemovals(res, e.tool); len(got) != 0 {
					t.Errorf("removed %q from the engine that replaced the recorded one", got)
				}
				// After the record names the new engine, never before.
				if lineAt(res, "--engine-replaced: 'target") < lineAt(res, "state: recorded the engine ENGINE-TWO") {
					t.Error("the lines left the record before it named the engine that replaced it")
				}
			})
		}
	})
}

// TestImageTagsNotRecorded: what the script does not record. On a
// Kubernetes target other than Rancher Desktop's - Docker Desktop's, whose
// one node sees the images of the Docker engine here (the route local) -
// a tag the build made is not recorded: that engine is not the target's.
func TestImageTagsNotRecorded(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		r = r.fresh(t)
		f := newImgFix(imgCommit)
		r.imageBinaries(imgCommit, "amd64")
		rules := []rule{kubeServer("https://kubernetes.docker.internal:6443"), tagAbsent("docker", f)}
		rules = append(rules, gitRules("")...)
		rules = append(rules, nodeRules("kubectl", "amd64")...)
		r.rules(append(rules, imgEngine("docker", "amd64", "amd64", f)...)...)
		res := r.image("KMS=local\nDEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\n",
			resolveAndDeliver+`; STATE=$TMPDIR/entry; mkdir "$STATE"; _engine_id=`+clusterUID+`; image_tags_record; ls -A "$STATE"; echo "listed"; rm -rf "$STATE"`, false)
		res.want(t, 0, "is on the Docker engine here, whose images the cluster's one node sees", "resolved "+f.ref()+" amd64\nlisted\n")
		if strings.Contains(res.stdout, "image: recorded") {
			t.Errorf("a tag on the engine of a Kubernetes target that is not Rancher Desktop's was recorded:\n%s", res.stdout)
		}
		if callIndex(res, "docker", "image", "tag") < 0 {
			t.Error("the image was not tagged: the case does not make a tag")
		}
	})
}

// TestImageTagsComposePurge: a purge of a compose cluster removes, after
// its volumes, every recorded tag of its engine that no container names,
// and keeps one a container names.
func TestImageTagsComposePurge(t *testing.T) {
	t.Parallel()
	const (
		old  = "cactusfs:0.7.0-411-0123456789ab"
		used = "cactusfs:0.7.0-412-feedfacefeed"
	)
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range []dockerEngine{dockerEngines[0], dockerEngines[2]} {
			t.Run(e.target, func(t *testing.T) {
				t.Parallel()
				r := clone(t, clusterInstalled(t, r, e, clusterEnv))
				state := r.stateOf(e.targetID)
				put(t, filepath.Join(state, "images"), "target "+old+" "+e.id+"\ntarget "+used+" "+e.id+"\n")
				s := runningCluster(t, e, state, nodeImage)
				s.purged = true
				r.rules(append([]rule{usedBy(e.tool, asRun(e.tool, used))}, s.rules(t)...)...)
				res := clusterRun(r, e, clusterEnv, "--uninstall", "--purge", "--purge-confirm=cactusfs")
				res.want(t, 0, "image: removed "+old+" on "+engineIdentityOf(e)+", which the script had made",
					"image: "+used+" is kept: a container on "+engineIdentityOf(e)+" still runs it", "purged: cactusfs")
				if got := imageRemovals(res, e.tool); !slices.Equal(got, []string{old}) {
					t.Errorf("removed %q, want %s alone", got, old)
				}
				vols := callIndex(res, e.tool, "volume", "rm", "cactusfs-config")
				if rm := callIndex(res, e.tool, "image", "rm", old); vols < 0 || rm < vols {
					t.Errorf("the config volume deleted at %d, the image at %d", vols, rm)
				}
				if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the entry is still there: %v", err)
				}
			})
		}
	})
}
