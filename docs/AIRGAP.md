# Air-gapped installation

ADR-0025 §3, OPERATIONS §25.3. For a Nexus installation with no route to
public registries, package repositories or the internet: build a bundle on a
connected host, carry it across the gap, verify it, mirror its images into a
private registry, and install the chart from that registry **by digest**.

## 1. Build the bundle (connected host)

Needs Docker (with buildx), helm, go and python3; uv and cosign are used when
installed, skopeo too (exact upstream bytes, no daemon).

```sh
make airgap-bundle AIRGAP_VERSION=1.4.0                        # the Docker host's platform
make airgap-bundle AIRGAP_VERSION=1.4.0 PLATFORMS=linux/amd64,linux/arm64
```

`deploy/airgap/build-bundle.py --plan` prints what would be bundled without
building anything. The image list is **derived**, not maintained by hand: the
chart is rendered with every optional component on (with and without PITR),
the compose files are resolved, every third-party reference is resolved to a
registry digest (an unresolvable tag, or a pinned digest the registry no
longer serves, fails the build), and the chart is rendered once more with every
image pointed at a sentinel registry to prove the generated values redirect
**all** of them. First-party images are built from the tree for the requested
platforms.

Output: `dist/nexus-airgap-<version>.tar.gz` and `.tar.gz.sha256`:

| Path | Contents |
|---|---|
| `images/*.tar` | one OCI image archive per image, named `nexus-airgap.local/<repo>:<tag>` so a `docker load` can never replace a real image |
| `images.tsv` | name, kind, archive, load name, bundle digest, target repository and tag, platforms, where it is used |
| `chart/kgf-nexus-<chart version>.tgz` | the packaged Helm chart |
| `bin/<os>-<arch>/` | `nexusctl` (linux/amd64, linux/arm64, darwin/arm64) and `nexus-agent` (linux/amd64, linux/arm64); `CGO_ENABLED=0`, `-trimpath` |
| `sdk/` | the Python SDK wheel |
| `schemas/` | the workflow JSON Schemas |
| `airgap/` | the values template and the chart probes `load.sh` renders with |
| `load.sh` | the mirror script |
| `manifest.json` | version, git commit, build time, tool versions, platforms, per-image source reference and digest, per-file sha256, and whether it is signed |
| `SHA256SUMS` | every file above |
| `SHA256SUMS.cosign.bundle` | when cosign was available: `cosign sign-blob` over `SHA256SUMS` (key from `COSIGN_KEY`, else keyless) |

An unsigned bundle says so (`"signed": false` with the reason). Then the
`.tar.gz.sha256`, obtained through a separate channel, is its integrity anchor.

## 2. Carry and verify (inside the gap)

```sh
sha256sum -c nexus-airgap-1.4.0.tar.gz.sha256
tar -xzf nexus-airgap-1.4.0.tar.gz && cd nexus-airgap-1.4.0
./load.sh --verify-only                                   # checksums (+ signature) only
COSIGN_PUBLIC_KEY=cosign.pub ./load.sh --verify-only --require-signature
```

`load.sh` refuses any file that `SHA256SUMS` does not list, verifies every
listed file, and, when the bundle is signed and cosign is installed, verifies
the signature (`COSIGN_VERIFY_ARGS` for keyless identities).
`--require-signature` makes an unsigned bundle, or a missing cosign, fatal.

## 3. Mirror and render values

```sh
docker login registry.corp.example            # or skopeo login
./load.sh --registry registry.corp.example/nexus
```

For each image: load and push (skopeo `copy --all` when installed, else
`docker load`/`tag`/`push`), then read back the digest **the registry**
serves. `docker push` may re-encode a manifest, so the bundle's digest and
the registry's can differ; the values file always pins what the registry
actually has, and a mismatch between the push output and the registry is
fatal. `airgap-pushed.tsv` records both digests.

`values-airgap.yaml` then points every chart image at
`registry.corp.example/nexus/<repo>@sha256:...`, and `load.sh` proves it:
`helm template` with every probe must render only images from that registry,
by digest.

## 4. Install and upgrade

```sh
helm upgrade --install nexus chart/kgf-nexus-*.tgz \
  -f my-values.yaml -f values-airgap.yaml       # values-airgap.yaml LAST, so it wins
```

Upgrades repeat 1-4 with the new bundle; migrations run from the mirrored
`migrate` image as usual (OPERATIONS §6). Keep the previous bundle until the
upgrade is verified: it is the rollback.

## 5. Test it

`make airgap-test` (after `make airgap-bundle`) starts a throwaway,
digest-pinned `registry:2` on localhost, runs the bundle's own `load.sh`
against it, re-renders the chart with every probe to confirm only
`localhost:<port>/nexus/...@sha256` images remain, checks the registry serves
every recorded digest, and pulls a few back after deleting the local copies.
`AIRGAP_TEST_PORT`, `AIRGAP_TEST_NAME` and `KEEP=1` adjust it.

## Not included

- **The platform underneath:** Kubernetes itself, the container runtime, a
  registry, PostgreSQL operators, the CSI Key Vault driver, OS packages, GPU
  drivers and the NVIDIA container toolkit. KGF Cloud's stamp provides these
  in its environments.
- **Task images.** Workflows run images their authors choose; mirror those
  separately and reference them by digest in specs.
- **Python dependencies of the SDK wheel** beyond the wheel itself: install
  them from an internal package mirror.
- **Other platforms** than the ones built: the default is the build host's
  platform only.
- **Vulnerability scan results and an SBOM**: not produced (a service-level
  gap, docs/SOC2-READINESS.md).
