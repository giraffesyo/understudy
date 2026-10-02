# Security policy

## Supported versions

Security fixes go into the latest release (and `canary`). understudy is
pre-1.0: older releases are not patched; upgrade to the latest.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/giraffesyo/understudy/security/advisories/new)
(Security tab -> "Report a vulnerability"), not in a public issue. Include
the version (`understudy version`), the playbook or command line that shows
the problem, and its impact.

You should get an acknowledgement within a week. Once a fix is released,
the advisory is published with credit to the reporter unless you prefer
otherwise.

Of particular interest: anything that lets a playbook, inventory, template
or managed host run code or read files on the controller beyond what
ansible-core allows, mishandling of Ansible Vault secrets or `no_log` data,
and weaknesses in the SSH transport or the agent protocol.

## Verifying release artifacts

Each release lists, beside the tarballs and the `.deb`/`.rpm`/`.apk`
packages, a CycloneDX SBOM per tarball, `checksums.txt` (SHA-256 of every
tarball, package and SBOM) and `checksums.txt.sigstore.json`, its keyless
cosign signature. All of it is made by the release workflow
(`.github/workflows/release.yml`, run from `canary`), which also records
GitHub build provenance attestations (SLSA, signed through Sigstore) for
every file in `checksums.txt` and for the container image, and an SBOM
attestation per tarball.

Check the signature on `checksums.txt`, then the files against it:

```sh
cosign verify-blob \
  --certificate-identity https://github.com/giraffesyo/understudy/.github/workflows/release.yml@refs/heads/canary \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

Or check a file's provenance (and a tarball's SBOM) with the GitHub CLI:

```sh
gh attestation verify understudy_v0.1.1_linux_amd64.tar.gz --repo giraffesyo/understudy
gh attestation verify understudy_0.1.0_linux_amd64.deb --repo giraffesyo/understudy
gh attestation verify understudy_v0.1.1_linux_amd64.tar.gz --repo giraffesyo/understudy \
  --predicate-type https://cyclonedx.org/bom
```

The image `ghcr.io/giraffesyo/understudy` is signed with cosign (keyless;
the signature is a Sigstore bundle, so verify with cosign 3 or newer —
cosign 2 reports "no signatures found") and carries a provenance
attestation in the registry:

```sh
cosign verify ghcr.io/giraffesyo/understudy:0.1.1 \
  --certificate-identity https://github.com/giraffesyo/understudy/.github/workflows/release.yml@refs/heads/canary \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/giraffesyo/understudy:0.1.1 --repo giraffesyo/understudy
```
