# Changelog

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
