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

Each release lists, beside the tarballs, `checksums.txt` (SHA-256 of every
tarball and SBOM) and a CycloneDX SBOM per tarball. The tarballs carry
GitHub build provenance attestations (SLSA, signed through Sigstore) and SBOM
attestations, made by the release workflow:

```sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify understudy_v0.1.0_linux_amd64.tar.gz --repo giraffesyo/understudy
gh attestation verify understudy_v0.1.0_linux_amd64.tar.gz --repo giraffesyo/understudy \
  --predicate-type https://cyclonedx.org/bom
```
