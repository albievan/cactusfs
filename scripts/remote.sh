#!/bin/sh
# Runs a make target on another machine: the tree is put there exactly as it
# is here - the commit that is checked out, with its history, and on top of
# it what the working tree has changed, added and removed - and the target
# runs in that machine's container engine. Nothing is started on this one.
#
#   REMOTE=user@host scripts/remote.sh check
#   REMOTE=user@host REMOTE_DIR=/srv/cactusfs-src scripts/remote.sh test-chaos VAR=value
#
# The machine needs make, git, tar and a container engine; ssh must work
# without a prompt. Which machine is the caller's business: no address is
# kept in this repository.
set -eu

: "${REMOTE:?set REMOTE=user@host}"
dir=${REMOTE_DIR:-cactusfs-src}
[ $# -ge 1 ] || { echo "usage: REMOTE=user@host $0 <make target> [VAR=value...]" >&2; exit 2; }

# Everything below is pasted into a command line for the other machine's
# shell: only what cannot mean anything to a shell is let through.
plain() {
	case $1 in
	*[!A-Za-z0-9_./=:@+,-]* | '' | -*) return 1 ;;
	esac
}
for arg in "$@" "$dir" "$REMOTE"; do
	# A value of a make variable may begin with a hyphen (an engine
	# option); a target, a name, a host and a directory may not.
	case $arg in
	*=*) name=${arg%%=*} value=${arg#*=} ;;
	*) name=$arg value=x ;;
	esac
	plain "$name" && { [ -z "${value#-}" ] || plain "x$value"; } || { echo "remote: '$arg' has characters this script does not pass to another machine" >&2; exit 2; }
done
case $dir in
/ | . | .. | */.. | */. | /root | /home | /etc* | /usr* | /var | /bin* | /boot*)
	echo "remote: refusing to use '$dir' as the directory of the copy" >&2
	exit 2
	;;
esac

head=$(git rev-parse --verify 'HEAD^{commit}')
id=$(date +%Y%m%d%H%M%S)-$$
run=.remote-run
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# 1. The history, so that the gates which compare with the last commit have
#    it: a bundle of HEAD.
git bundle create -q "$work/head.bundle" HEAD
# 2. The working tree as git sees it - tracked and untracked, not what is
#    ignored - less the files that are tracked and gone. What is not
#    committed is said, so that nobody takes a green run for a judgement of
#    the commit alone.
untracked=$(git ls-files -o --exclude-standard | wc -l | tr -d ' ')
changed=$(git status --porcelain | wc -l | tr -d ' ')
echo "remote: commit $head with $changed uncommitted changes, $untracked of them files git does not track"
git ls-files -d -z >"$work/deleted"
git ls-files -co --exclude-standard -z | python3 -c '
import sys
gone = set(open(sys.argv[1], "rb").read().split(b"\0"))
sys.stdout.buffer.write(b"\0".join(p for p in sys.stdin.buffer.read().split(b"\0") if p and p not in gone) + b"\0")
' "$work/deleted" >"$work/present"
COPYFILE_DISABLE=1 tar --null -T "$work/present" -cf "$work/tree.tar"
# What the other machine must not keep: every file of the commit that is not
# in the tree as it is sent. A deletion that was added to the index is no
# longer "tracked and gone" to git, and is still gone.
# The list is read there a line at a time and each line removed, so a name
# that would be read as another one is refused here, and with it the run: a
# name with a newline (git separates them with NUL, so it can hold one; a NUL
# it cannot), one that begins at the root, and one that climbs with "..".
# Nothing has been sent when this fails.
git ls-tree -r --name-only -z HEAD | python3 -c '
import sys
present = set(open(sys.argv[1], "rb").read().split(b"\0"))
refused = 0
for p in sys.stdin.buffer.read().split(b"\0"):
    if not p or p in present:
        continue
    if b"\n" in p or p.startswith(b"/") or b".." in p.split(b"/"):
        sys.stderr.write("remote: %r is not a name this script removes on another machine\n" % p)
        refused = 1
        continue
    sys.stdout.buffer.write(p + b"\n")
sys.exit(3 if refused else 0)
' "$work/present" >"$work/deleted.txt"

# The directory is this script's own: made by it, marked by it, and used by
# one run at a time. The lock is a directory, and whoever makes it has it:
# of two starts at one moment, one mkdir fails. A run writes its process ID
# when it begins and takes it away, and then the lock, when it ends, so that
# what a later start finds says what is going on:
#   the lock and no process ID      a start that has not got to its run yet
#                                   (or one that failed and could not say so)
#   the lock and a live process     a run
#   the lock and a dead process     a run that ended without a word. Its lock
#                                   is taken over by whoever takes its
#                                   process ID away - a rename, which one of
#                                   two can do.
# A start that fails after it has the lock removes it too.
ssh "$REMOTE" "set -e
	if [ -e '$dir' ] && [ ! -e '$dir/.cactusfs-remote' ] && [ -n \"\$(ls -A '$dir' 2>/dev/null)\" ]; then
		echo \"remote: $dir exists and was not made by this script\" >&2; exit 3
	fi
	mkdir -p '$dir' && cd '$dir' && touch .cactusfs-remote
	if mkdir $run.lock 2>/dev/null; then
		rm -f $run.pid
	elif [ ! -e $run.pid ]; then
		echo 'remote: another start is using this directory - or one failed: if none is going, remove $run.lock there' >&2; exit 4
	elif kill -0 \"\$(cat $run.pid)\" 2>/dev/null; then
		echo 'remote: another run is using this directory' >&2; exit 4
	elif mv $run.pid $run.pid.$id 2>/dev/null; then
		rm -f $run.pid.$id
	else
		echo 'remote: another start is using this directory' >&2; exit 4
	fi
	[ -d .git ] || git init -q
	mkdir -p .git/info && printf '%s\n' '.cactusfs-remote' '$run.*' >.git/info/exclude"
# The lock is this start's until the run has it: if the copy fails, it is
# given back.
locked=1
trap 'rm -rf "$work"; [ "$locked" = 0 ] || ssh "$REMOTE" "rmdir '"'$dir/$run.lock'"'" || echo "remote: could not remove $dir/$run.lock on $REMOTE; remove it there" >&2' EXIT
scp -q "$work/head.bundle" "$work/tree.tar" "$work/deleted.txt" "$REMOTE:$dir/.git/"
locked=0
# The commit, exactly; then nothing that git does not ignore but what the
# archive brings; then what was removed here, removed there. Build output and
# caches, which git ignores, stay.
ssh "$REMOTE" "set -e; cd '$dir'
	started=
	trap '[ -n \"\$started\" ] || rmdir $run.lock' EXIT
	git fetch -q .git/head.bundle HEAD
	git checkout -q -f --detach $head
	git clean -q -fd
	while IFS= read -r gone; do [ -z \"\$gone\" ] || rm -rf -- \"\$gone\"; done <.git/deleted.txt
	tar -xf .git/tree.tar
	rm -f .git/head.bundle .git/tree.tar .git/deleted.txt $run.status
	nohup sh -c 'echo \$\$ >$run.pid; make $*; echo \$? >$run.status.tmp; mv $run.status.tmp $run.status; rm -f $run.pid; rmdir $run.lock' >$run.$id.log 2>&1 </dev/null &
	started=1"

# The target runs detached from any connection, which may not outlive it;
# its output and exit status are fetched as it goes.
seen=0
failures=0
while :; do
	sleep 10
	# One connection a round: how much the log holds, that much of it from
	# where the last round ended, and how the run stands.
	# The state is read before the log is measured, so that the end of the
	# log of a run that has just ended is not missed.
	if ! out=$(ssh -o ConnectTimeout=20 -o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=2 "$REMOTE" "cd '$dir'
		if [ -e $run.status ]; then state=\$(cat $run.status)
		elif kill -0 \"\$(cat $run.pid)\" 2>/dev/null; then state=running
		else state=lost; fi
		size=\$(wc -c <$run.$id.log | tr -d ' ')
		head -c \"\$size\" $run.$id.log | tail -c +$((seen + 1))
		printf '\n%s %s\n' \"\$size\" \"\$state\""); then
		failures=$((failures + 1))
		[ "$failures" -lt 30 ] || { echo "remote: no connection to $REMOTE for five minutes; the run may still be going" >&2; exit 1; }
		continue
	fi
	failures=0
	last=$(printf '%s\n' "$out" | tail -n 1)
	printf '%s\n' "$out" | sed '$d' | sed '${/^$/d;}'
	seen=${last%% *}
	case ${last##* } in
	running) ;;
	lost)
		echo "remote: the run ended without saying how" >&2
		exit 1
		;;
	*) exit "${last##* }" ;;
	esac
done
