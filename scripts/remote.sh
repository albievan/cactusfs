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
	*[!A-Za-z0-9_./=:@+,-]* | '') return 1 ;;
	esac
}
for arg in "$@" "$dir" "$REMOTE"; do
	plain "$arg" || { echo "remote: '$arg' has characters this script does not pass to another machine" >&2; exit 2; }
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
#    ignored - less the files that are tracked and gone.
git ls-files -d -z >"$work/deleted"
git ls-files -co --exclude-standard -z | python3 -c '
import sys
gone = set(open(sys.argv[1], "rb").read().split(b"\0"))
sys.stdout.buffer.write(b"\0".join(p for p in sys.stdin.buffer.read().split(b"\0") if p and p not in gone) + b"\0")
' "$work/deleted" >"$work/present"
COPYFILE_DISABLE=1 tar --null -T "$work/present" -cf "$work/tree.tar"
tr '\0' '\n' <"$work/deleted" >"$work/deleted.txt"

# The directory is this script's own: made by it, marked by it, and used by
# one run at a time.
ssh "$REMOTE" "set -e
	if [ -e '$dir' ] && [ ! -e '$dir/.cactusfs-remote' ] && [ -n \"\$(ls -A '$dir' 2>/dev/null)\" ]; then
		echo \"remote: $dir exists and was not made by this script\" >&2; exit 3
	fi
	mkdir -p '$dir' && cd '$dir' && touch .cactusfs-remote
	if ! mkdir $run.lock 2>/dev/null; then
		if [ -e $run.pid ] && kill -0 \"\$(cat $run.pid)\" 2>/dev/null; then
			echo 'remote: another run is using this directory' >&2; exit 4
		fi
	fi
	[ -d .git ] || git init -q
	mkdir -p .git/info && printf '%s\n' '.cactusfs-remote' '$run.*' >.git/info/exclude"
scp -q "$work/head.bundle" "$work/tree.tar" "$work/deleted.txt" "$REMOTE:$dir/.git/"
# The commit, exactly; then nothing that git does not ignore but what the
# archive brings; then what was removed here, removed there. Build output and
# caches, which git ignores, stay.
ssh "$REMOTE" "set -e; cd '$dir'
	git fetch -q .git/head.bundle HEAD
	git checkout -q -f --detach $head
	git clean -q -fd
	tar -xf .git/tree.tar
	while IFS= read -r gone; do [ -z \"\$gone\" ] || rm -f -- \"\$gone\"; done <.git/deleted.txt
	rm -f .git/head.bundle .git/tree.tar .git/deleted.txt $run.status
	nohup sh -c 'make $*; echo \$? >$run.status; rmdir $run.lock' >$run.$id.log 2>&1 </dev/null &
	echo \$! >$run.pid"

# The target runs detached from any connection, which may not outlive it;
# its output and exit status are fetched as it goes.
seen=0
failures=0
while :; do
	sleep 10
	# One connection a round: how much the log holds, that much of it from
	# where the last round ended, and how the run stands.
	if ! out=$(ssh -o ConnectTimeout=20 "$REMOTE" "cd '$dir'
		size=\$(wc -c <$run.$id.log | tr -d ' ')
		head -c \"\$size\" $run.$id.log | tail -c +$((seen + 1))
		if [ -e $run.status ]; then state=\$(cat $run.status)
		elif kill -0 \"\$(cat $run.pid)\" 2>/dev/null; then state=running
		else state=lost; fi
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
