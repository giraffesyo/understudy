# Changelog

## [0.2.1](https://github.com/giraffesyo/understudy/compare/v0.2.0...v0.2.1) (2026-10-05)


### Bug Fixes

* **connection:** preserve immediate privilege escalation replies ([8dc414f](https://github.com/giraffesyo/understudy/commit/8dc414fdb8170e24dcaccd8008454b49329a6442))
* **modules:** resolve NSS users and groups without cgo ([5a3b59f](https://github.com/giraffesyo/understudy/commit/5a3b59f8bf50d85f75e489cf75942e36a82ba6ce))

## [0.2.0](https://github.com/giraffesyo/understudy/compare/v0.1.2...v0.2.0) (2026-10-02)


### Features

* **api:** RunFiles runs YAML playbooks in-process, configured like ansible-playbook ([6438226](https://github.com/giraffesyo/understudy/commit/64382261b290cdd53b69ae68cf958e3485ff52d6))

## [0.1.2](https://github.com/giraffesyo/understudy/compare/v0.1.1...v0.1.2) (2026-10-02)


### Bug Fixes

* **connection:** a sudo password prompt fails the task as "Missing sudo password" ([79b8baf](https://github.com/giraffesyo/understudy/commit/79b8baf07064ceb3d89a85adf844b405bb02d4e9))
* **modules:** a failed DNS lookup is EAI_NONAME when nsswitch asks files or myhostname after dns ([534924c](https://github.com/giraffesyo/understudy/commit/534924cc75503a35b18877c73cc785a2f4f73249))
* **modules:** a lame DNS referral fails getaddrinfo with EAI_AGAIN, as glibc reports it ([d798c0a](https://github.com/giraffesyo/understudy/commit/d798c0a774e3678c21babcc786c78672dd54f004))
* **modules:** know CPython 3.14.8's _ssl.c lines ([655eae2](https://github.com/giraffesyo/understudy/commit/655eae284dca086e0c2c974b1359f2f232707990))
* **modules:** mask credentials in URLs as ansible-core 2.21.4 does ([ac30231](https://github.com/giraffesyo/understudy/commit/ac30231876d96ce4c8070faef466079e05dbb769))
* **modules:** sys.version's branch follows the CPython version, not a "main" string in the binary ([06da2c0](https://github.com/giraffesyo/understudy/commit/06da2c0eb61f97b1a00f0577be748e556ca5d905))
* **modules:** tempfile rejects a prefix or suffix with a path separator ([410da4c](https://github.com/giraffesyo/understudy/commit/410da4ccbf4e478f61d6fba2d50ddf2199d1531b))
* **template:** password_hash through libxcrypt matches ansible-core on Linux ([d5f20c0](https://github.com/giraffesyo/understudy/commit/d5f20c0d980a010cb6036384479588d3e1e5473c))


### Documentation

* **readme:** what differs against ansible-core 2.21.3 ([dfeb3d8](https://github.com/giraffesyo/understudy/commit/dfeb3d8f6d90d5621bc76b0d842f2c88ffa6709e))
* verify examples use v0.1.1; image signatures need cosign 3 ([164be9d](https://github.com/giraffesyo/understudy/commit/164be9dc3bf97da3b70c681237b400022e8960b9))

## [0.1.1](https://github.com/giraffesyo/understudy/compare/v0.1.0...v0.1.1) (2026-10-02)


### Bug Fixes

* **facts:** concurrent fact updates no longer race on the cache ([b30b599](https://github.com/giraffesyo/understudy/commit/b30b5995fb95f4d55a2750f184f69523e4ce1b43))


### Build System

* **release:** GoReleaser builds the artifacts, packages and image ([0577a7c](https://github.com/giraffesyo/understudy/commit/0577a7c0161c19fe06d800873b6710a3ca822a9a))

## 0.1.0 (2026-10-01)


### Features

* **cli:** UNDERSTUDY_CPUPROFILE writes a CPU profile of the run ([6a9e304](https://github.com/giraffesyo/understudy/commit/6a9e3048bbf3db866c0f2faa276f0bb05dbf2cd7))


### Bug Fixes

* **deps:** bump golang.org/x/crypto to v0.56.0 ([91cae65](https://github.com/giraffesyo/understudy/commit/91cae657089f4d732163983aafaa65c7804536cf))


### Build System

* make sbom writes a CycloneDX SBOM per release tarball ([7e447ad](https://github.com/giraffesyo/understudy/commit/7e447adb004badddb784038e243d775e3ef6902b))
* **release:** cut releases with release-please on main ([cae251e](https://github.com/giraffesyo/understudy/commit/cae251efa4bdfc9a51670df7142c1aa711bbb87d))
