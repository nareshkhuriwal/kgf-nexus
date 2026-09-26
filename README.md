# KGF Nexus

A reusable, application-agnostic distributed workflow orchestration platform.

Two planes, one narrow contract. The control plane decides *what* runs *where*;
the execution plane is a fleet of lightweight agents that register, heartbeat,
pull leased work, run it, and report results. Postgres is the source of truth,
including the dispatch queue.

## Quick start

Needs Docker (Compose v2) and Go. From a fresh checkout:

```bash
make quickstart    # = go run ./cli/cmd/nexusctl up
```

It starts the local stack, registers a tenant, mints an admin key and the
agents' worker token through the API, and prints the console URL and
credentials once. `go run ./cli/cmd/nexusctl down --volumes` removes the stack
and its local record again. `make up` is the path
for driving Compose with your own keys (deploy/README.md).

## Documentation

| Document | What it covers |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | The full design: component architecture, data model, state machines, execution semantics, failure matrix |
| [docs/adr/](docs/adr/README.md) | Decision records, each with alternatives and the reason for rejection |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Running and operating a deployment |
| [docs/TLS.md](docs/TLS.md) | Transport security setup |
| [cli/README.md](cli/README.md) | The `nexus` command-line client |
| [sdk/python/README.md](sdk/python/README.md) | The Python SDK for authoring workflows |
| [ui/README.md](ui/README.md) | The web UI |
| [examples/](examples/README.md) | Runnable example workflows |

## License

Copyright 2026 Khuriwal Group

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at:

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.

The full license text is in [LICENSE](LICENSE), and attribution notices for
bundled third-party software are in [NOTICE](NOTICE). Source files carry an
SPDX identifier (`SPDX-License-Identifier: Apache-2.0`) rather than a full
header.

A per-dependency license inventory and the Apache-2.0 compatibility assessment
are in [docs/THIRD-PARTY-LICENSES.md](docs/THIRD-PARTY-LICENSES.md). All
dependencies are under permissive licenses (Apache-2.0, MIT, ISC, BSD-2-Clause,
BSD-3-Clause); none are copyleft.

### Contributing

Contributions are accepted under the terms of the Apache License, Version 2.0,
as stated in section 5 of the License: any contribution intentionally submitted
for inclusion in this work shall be under those terms, without additional
conditions.
