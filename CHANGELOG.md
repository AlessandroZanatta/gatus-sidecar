# Changelog

## [0.5.3](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.5.2...v0.5.3) (2026-08-17)


### Bug Fixes

* drop the inferred check when its Service is monitored directly ([12ae3b6](https://github.com/AlessandroZanatta/gatus-sidecar/commit/12ae3b620e89b185142143baf2c666dd76ebef46))


### Documentation

* cover path naming, skipped backends and warning semantics ([19c1f48](https://github.com/AlessandroZanatta/gatus-sidecar/commit/19c1f48f49a7fabaae33f5ce898d0585621ffa1b))

## [0.5.2](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.5.1...v0.5.2) (2026-08-17)


### Bug Fixes

* stop dropping a second path on one host, and quieten expected drops ([6d67917](https://github.com/AlessandroZanatta/gatus-sidecar/commit/6d679178f8d9e4e05f99526fc3538b63bd0a616a))

## [0.5.1](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.5.0...v0.5.1) (2026-08-17)


### Bug Fixes

* skip backends that are not Kubernetes Services ([ed769a1](https://github.com/AlessandroZanatta/gatus-sidecar/commit/ed769a124418990cea89333597fca0a22306a3c7))

## [0.5.0](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.4.0...v0.5.0) (2026-08-16)


### Features

* discover IngressRouteTCP endpoints and their entrypoint ports ([484e5b8](https://github.com/AlessandroZanatta/gatus-sidecar/commit/484e5b89a8c811a49679a8992ad003d8d62c99b9))

## [0.4.0](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.3.0...v0.4.0) (2026-08-16)


### Features

* add exclude annotation, fix the annotation prefix ([0759024](https://github.com/AlessandroZanatta/gatus-sidecar/commit/0759024f65ccdd63a4c13ced13590eec1a1186eb))
* gatus config sidecar driven by annotations and templates ([19e047f](https://github.com/AlessandroZanatta/gatus-sidecar/commit/19e047f6a676be13a9236a27f84ad4c07c95de2b))
* make the crd consumable as a kustomize remote resource ([b87e72a](https://github.com/AlessandroZanatta/gatus-sidecar/commit/b87e72a45f2f578283eceefb79250e8ed8335f3d))


### Bug Fixes

* build the arm64 image for arm64 ([723cd6f](https://github.com/AlessandroZanatta/gatus-sidecar/commit/723cd6f13cd847aa3f300c75c29166439345b077))
* publish a complete configuration before Gatus first reloads ([17c1b5c](https://github.com/AlessandroZanatta/gatus-sidecar/commit/17c1b5c779262741eb93563292bb08d42853b2f5))
* release the first version as 0.1.0 rather than 1.0.0 ([69a217e](https://github.com/AlessandroZanatta/gatus-sidecar/commit/69a217ef753c1bbee6d4abbca4f399da79791d1d))
* update crd with new kubebuilder version ([e17d28c](https://github.com/AlessandroZanatta/gatus-sidecar/commit/e17d28c13046c20ce4b584d77592ff646087e253))
* update go modules ([67ca332](https://github.com/AlessandroZanatta/gatus-sidecar/commit/67ca332a1c13c42c1fd2f7f55e2147f112db885c))

## [0.3.0](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.2.0...v0.3.0) (2026-08-16)


### Features

* add exclude annotation, fix the annotation prefix ([856dcef](https://github.com/AlessandroZanatta/gatus-sidecar/commit/856dcefc83ec40afbf9deac5d5a80be27e2111d5))


### Bug Fixes

* update crd with new kubebuilder version ([b1db54c](https://github.com/AlessandroZanatta/gatus-sidecar/commit/b1db54c20156e5bbfca8d3e9d5814846f71484f6))
* update go modules ([bebeedd](https://github.com/AlessandroZanatta/gatus-sidecar/commit/bebeedd0468017bd1bbc09ea0a364fb92f17c9ed))

## [0.2.0](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.1.1...v0.2.0) (2026-08-16)


### Features

* make the crd consumable as a kustomize remote resource ([b87e72a](https://github.com/AlessandroZanatta/gatus-sidecar/commit/b87e72a45f2f578283eceefb79250e8ed8335f3d))

## [0.1.1](https://github.com/AlessandroZanatta/gatus-sidecar/compare/v0.1.0...v0.1.1) (2026-08-16)


### Bug Fixes

* build the arm64 image for arm64 ([723cd6f](https://github.com/AlessandroZanatta/gatus-sidecar/commit/723cd6f13cd847aa3f300c75c29166439345b077))

## 0.1.0 (2026-08-16)


### Features

* gatus config sidecar driven by annotations and templates ([19e047f](https://github.com/AlessandroZanatta/gatus-sidecar/commit/19e047f6a676be13a9236a27f84ad4c07c95de2b))


### Bug Fixes

* release the first version as 0.1.0 rather than 1.0.0 ([69a217e](https://github.com/AlessandroZanatta/gatus-sidecar/commit/69a217ef753c1bbee6d4abbca4f399da79791d1d))
