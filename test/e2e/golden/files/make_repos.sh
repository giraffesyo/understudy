#!/bin/sh
# Builds the local repositories the git golden playbook clones from, with
# fixed identities and dates so every commit/tag hash is the same on every
# run:
#   <dir>/upstream   main: c1 (tag v1) -> c2 (annotated tag v2); feature: c3
#   <dir>/sub        a repository used as a submodule
#   <dir>/withsub    main: one commit adding sub at libs/sub
#   <dir>/signed     main: one commit signed with gpg_signer.asc (a
#                    test-only key); <dir>/gnupg holds its public key
# "advance" adds a commit to upstream/main and to sub, then copies
# upstream to <dir>/mirror (same history, another URL).
set -e
d=$1
signer=$(cd "$(dirname "$0")" && pwd)/gpg_signer.asc
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

rm -rf "$d/upstream" "$d/sub" "$d/withsub" "$d/mirror" "$d/signed" "$d/gnupg"

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

# The signing keyring lives under /tmp (gpg-agent's socket path must stay
# short); the signature's time is fixed, so the commit hash is too.
if command -v gpg >/dev/null 2>&1; then
  g=$(mktemp -d /tmp/gpgXXXXXX)
  GNUPGHOME=$g gpg --batch --quiet --import "$signer" 2>/dev/null
  printf '#!/bin/sh\nexec gpg --faked-system-time 20260101T000000 "$@"\n' > "$g/gpg.sh"
  chmod +x "$g/gpg.sh"
  git init -q -b main "$d/signed"
  cd "$d/signed"
  printf 'signed\n' > s.txt
  git add s.txt
  GNUPGHOME=$g git -c gpg.program="$g/gpg.sh" -c user.signingkey=golden@example.com commit -q -S -m signed 2>/dev/null
  mkdir -m 700 "$d/gnupg"
  GNUPGHOME=$g gpg --batch --armor --export golden@example.com |
    GNUPGHOME="$d/gnupg" gpg --batch --quiet --import 2>/dev/null
  GNUPGHOME=$g gpgconf --kill gpg-agent 2>/dev/null || true
  rm -rf "$g"
fi
