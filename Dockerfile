# The release image, built by GoReleaser (dockers_v2 in .goreleaser.yaml)
# from the binaries it already built; it is not a standalone build. The
# build context holds <os>/<arch>/understudy for every platform.
#
# Alpine rather than scratch/distroless: understudy's SSH client is native
# Go (no openssh needed), but the local connection (hosts: localhost,
# delegate_to: localhost), `pipe` lookups and ProxyCommand run through
# /bin/sh, which distroless/static lacks. Alpine's base has /bin/sh and the
# CA bundle and nothing else of note. Pinned by digest (dependabot bumps it).
FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/understudy /usr/bin/understudy

WORKDIR /work
ENTRYPOINT ["/usr/bin/understudy"]
CMD ["--help"]
