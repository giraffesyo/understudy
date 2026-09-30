#!/bin/sh
# Builds the local repositories the git golden playbook clones from, with
# fixed identities and dates so every commit/tag hash is the same on every
# run:
#   <dir>/upstream   main: c1 (tag v1) -> c2 (annotated tag v2); feature: c3
#   <dir>/sub        a repository used as a submodule
#   <dir>/withsub    main: one commit adding sub at libs/sub
# "advance" adds a commit to upstream/main and to sub, then copies
# upstream to <dir>/mirror (same history, another URL).
set -e
d=$1
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=Golden GIT_AUTHOR_EMAIL=golden@example.com
export GIT_COMMITTER_NAME=Golden GIT_COMMITTER_EMAIL=golden@example.com
export GIT_AUTHOR_DATE=2026-01-01T00:00:00Z GIT_COMMITTER_DATE=2026-01-01T00:00:00Z

if [ "$2" = advance ]; then
  export GIT_AUTHOR_DATE=2026-01-02T00:00:00Z GIT_COMMITTER_DATE=2026-01-02T00:00:00Z
  cd "$d/upstream"
  printf 'one\ntwo\nthree\n' > a.txt
  git commit -q -am c4
  cd "$d/sub"
  printf 'sub2\n' > s.txt
  git commit -q -am sub2
  rm -rf "$d/mirror"
  cp -R "$d/upstream" "$d/mirror"
  exit 0
fi

rm -rf "$d/upstream" "$d/sub" "$d/withsub" "$d/mirror"

git init -q -b main "$d/sub"
cd "$d/sub"
printf 'sub1\n' > s.txt
git add s.txt
git commit -q -m sub1

git init -q -b main "$d/upstream"
cd "$d/upstream"
printf 'one\n' > a.txt
git add a.txt
git commit -q -m c1
git tag v1
printf 'one\ntwo\n' > a.txt
git commit -q -am c2
git tag -a v2 -m 'release v2'
git checkout -q -b feature
printf 'feature\n' > f.txt
git add f.txt
git commit -q -m c3
git checkout -q main

git init -q -b main "$d/withsub"
cd "$d/withsub"
printf 'top\n' > top.txt
git add top.txt
# A relative URL keeps .gitmodules (and so the commit hash) independent
# of <dir>; it resolves against the superproject's own location.
git -c protocol.file.allow=always submodule add -q ../sub libs/sub
git commit -q -m 'add sub'
