# Third-Party Licenses

KGF Nexus is licensed under the Apache License, Version 2.0. This document
inventories the licenses of its third-party dependencies and records the
compatibility assessment behind that choice.

Regenerate the Go portion with `make license-report` (see the Makefile target).

## Compatibility summary

All dependencies are distributed under permissive licenses -- Apache-2.0, MIT,
ISC, BSD-2-Clause, and BSD-3-Clause. Every one of these is compatible with
distributing this project under Apache-2.0.

No dependency is under a copyleft license (GPL, LGPL, AGPL, MPL, EPL, or
SSPL), so no dependency imposes a reciprocal source-disclosure obligation on
KGF Nexus.

## Go dependencies (compiled into the service binaries)

Scope: modules reachable from `go list -deps ./...`. Build- and lint-only
tooling in the module graph is excluded because it is not linked into any
shipped artifact.

| Module | Version | License |
| --- | --- | --- |
| github.com/cespare/xxhash/v2 | v2.3.0 | MIT |
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/jackc/pgpassfile | v1.0.0 | MIT |
| github.com/jackc/pgservicefile | v0.0.0-20240606120523-5a60cdf6a761 | MIT |
| github.com/jackc/pgx/v5 | v5.11.0 | MIT |
| github.com/jackc/puddle/v2 | v2.2.2 | MIT |
| github.com/klauspost/compress | v1.19.2 | Apache-2.0 |
| github.com/klauspost/cpuid/v2 | v2.4.0 | MIT |
| github.com/klauspost/crc32 | v1.3.0 | BSD-3-Clause |
| github.com/minio/crc64nvme | v1.1.1 | Apache-2.0 |
| github.com/minio/md5-simd | v1.1.2 | Apache-2.0 |
| github.com/minio/minio-go/v7 | v7.3.0 | Apache-2.0 |
| github.com/philhofer/fwd | v1.2.0 | MIT |
| github.com/rs/xid | v1.6.0 | MIT |
| github.com/santhosh-tekuri/jsonschema/v6 | v6.0.3 | Apache-2.0 |
| github.com/tinylib/msgp | v1.6.4 | MIT |
| github.com/zeebo/xxh3 | v1.1.0 | BSD-2-Clause |
| go.yaml.in/yaml/v3 | v3.0.5 | Apache-2.0 |
| golang.org/x/crypto | v0.55.0 | BSD-3-Clause |
| golang.org/x/net | v0.58.0 | BSD-3-Clause |
| golang.org/x/sync | v0.22.0 | BSD-3-Clause |
| golang.org/x/sys | v0.47.0 | BSD-3-Clause |
| golang.org/x/text | v0.41.0 | BSD-3-Clause |
| google.golang.org/genproto/googleapis/rpc | v0.0.0-20260526163538-3dc84a4a5aaa | Apache-2.0 |
| google.golang.org/grpc | v1.83.2 | Apache-2.0 |
| google.golang.org/protobuf | v1.36.12 | BSD-3-Clause |
| gopkg.in/ini.v1 | v1.67.3 | Apache-2.0 |
| gopkg.in/yaml.v3 | v3.0.1 | Apache-2.0 |

Note: `gopkg.in/yaml.v3` is dual-licensed. The files ported from libyaml
(`apic.go`, `emitterc.go`, `parserc.go`, `readerc.go`, `scannerc.go`,
`writerc.go`, `yamlh.go`, `yamlprivateh.go`) remain under the MIT license; the
remainder is Apache-2.0. Both halves are compatible with Apache-2.0.

## Python SDK dependencies

The SDK (`sdk/python`) declares `license = {text = "Apache-2.0"}` and has **no
runtime dependencies** by design -- it is installed into every task image, and
each dependency would be a version conflict waiting to happen against the
user's own requirements.

Optional extras: `PyYAML` (MIT), used only to render a compiled spec; `pytest`
(MIT), development only.

## UI dependencies (`ui/`)

The UI is a Vite + React application. Its dependency tree resolves to the
following license distribution:

| License | Packages |
| --- | --- |
| MIT | 162 |
| ISC | 8 |
| Apache-2.0 | 5 |
| BSD-3-Clause | 2 |
| BSD-2-Clause | 2 |
| MIT-0 | 1 |
| CC-BY-4.0 | 1 |

The single CC-BY-4.0 package is `caniuse-lite`, a browser-compatibility **data
table** consumed by Browserslist/autoprefixer at build time. It is not linked
into or redistributed as part of the built UI bundle, and CC-BY-4.0 permits
redistribution with attribution in any case. No action is required.

`MIT-0` (`@csstools/color-helpers`) is MIT without the attribution clause --
strictly more permissive than MIT.

## Test data (not distributed in any binary)

`cli/internal/importer/testdata/upstream/` holds unmodified copies of example
pipelines used to test `nexusctl import`. They are read by tests only, never
compiled, embedded or shipped. Each directory's README gives the upstream URL,
tag, commit and per-file sha256.

| Project | Tag | Files | License |
| --- | --- | --- | --- |
| Apache Airflow (github.com/apache/airflow) | 3.3.2 | 8 example DAGs | Apache-2.0 (its NOTICE is reproduced in the directory README) |
| Argo Workflows (github.com/argoproj/argo-workflows) | v4.1.4 | 18 examples | Apache-2.0 |
| Prefect (github.com/PrefectHQ/prefect) | 3.8.6 | 4 examples | Apache-2.0 |

## Assessment

No license conflicts were identified. KGF Nexus may be distributed under the
Apache License, Version 2.0.
