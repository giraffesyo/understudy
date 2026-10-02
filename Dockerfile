# The release image, built by GoReleaser (dockers_v2 in .goreleaser.yaml)
# from the binaries it already built; it is not a standalone build. The
# build context holds <os>/<arch>/understudy for every platform.
#
# Alpine rather than scratch/distroless: understudy's SSH client is native
# Go (no openssh needed), but the local connection (hosts: localhost,
# delegate_to: localhost), `pipe` lookups and ProxyCommand run through
# /bin/sh, which distroless/static lacks. Alpine's base has /bin/sh and the
# CA bundle and nothing else of note. Pinned by digest (dependabot bumps it).
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/understudy /usr/bin/understudy

WORKDIR /work
ENTRYPOINT ["/usr/bin/understudy"]
CMD ["--help"]
