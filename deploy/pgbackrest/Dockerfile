# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Postgres + pgBackRest: the image the PITR overlay (deploy/docker-compose.pitr.yml)
# and the Helm chart (postgres.pitr.enabled) run instead of stock postgres.
#
# Why pgBackRest, briefly (the long form is docs/OPERATIONS.md §4.1):
#   * restore --type=time/lsn picks the right base backup for the target by
#     itself -- the operator names a moment, not a backup set;
#   * repository encryption (aes-256-cbc) and retention/expiry are built in and
#     run after every backup, rather than being a second tool on a cron;
#   * `pgbackrest verify` re-checksums every backup file and WAL segment in the
#     repository, which is what makes "the backups are good" a checkable claim;
#   * it is widely deployed for self-managed Postgres (Crunchy PGO is built on
#     it) and its releases track Postgres majors (2.59 already supports 19).
# The cost: its S3 client is TLS-only, so the dev backup store runs with a
# (generated) certificate -- which is how production object storage works anyway.
#
# Built from the upstream release tarball rather than `apk add pgbackrest`: an apk
# pin (`pgbackrest=2.58.0-r0`) stops resolving the day Alpine publishes -r1, and
# the distro lags upstream by a minor. The tarball digest is pinned below.
#
# PG_IMAGE must be the SAME major (and libc) as the server whose data directory
# this image will run: a physical backup is only restorable into the major that
# wrote it, and musl-vs-glibc collation differences corrupt text indexes. The
# stock compose/Helm server is postgres:16-alpine; this pins its current point.
ARG PG_IMAGE=postgres:16.15-alpine3.24

FROM ${PG_IMAGE} AS build
ARG PGBACKREST_VERSION=2.59.1
ARG PGBACKREST_SHA256=ca1e75c7490989a2fb39b8266c0f3c518dd3c873ddc0c3a346ca9b5dccc16455
RUN apk add --no-cache \
      build-base meson ninja pkgconf \
      openssl-dev libxml2-dev lz4-dev zstd-dev bzip2-dev zlib-dev yaml-dev \
      libpq-dev libssh2-dev \
 && wget -q -O /tmp/pgbackrest.tar.gz \
      "https://github.com/pgbackrest/pgbackrest/archive/refs/tags/release/${PGBACKREST_VERSION}.tar.gz" \
 && echo "${PGBACKREST_SHA256}  /tmp/pgbackrest.tar.gz" | sha256sum -c - \
 && mkdir /src && tar -xzf /tmp/pgbackrest.tar.gz -C /src --strip-components=1 \
 && meson setup /build /src --buildtype=release -Dlibssh2=enabled -Dlibzstd=enabled \
 && ninja -C /build \
 && /build/src/pgbackrest version

FROM ${PG_IMAGE}
ARG PGBACKREST_VERSION=2.59.1
LABEL org.opencontainers.image.title="kgf-nexus-postgres-pitr" \
      org.opencontainers.image.description="Postgres with pgBackRest ${PGBACKREST_VERSION} for continuous WAL archiving" \
      io.kgf-nexus.pgbackrest.version="${PGBACKREST_VERSION}"
# Runtime libraries only. jq is for the scheduler's reading of `info --output=json`;
# openssl is for the dev overlay's certificate generation and nothing else.
RUN apk add --no-cache \
      libxml2 lz4-libs zstd-libs libbz2 zlib yaml libpq libssh2 openssl jq \
 && mkdir -p /var/log/pgbackrest /var/spool/pgbackrest /var/lib/pgbackrest /etc/pgbackrest \
 && chown -R postgres:postgres /var/log/pgbackrest /var/spool/pgbackrest /var/lib/pgbackrest /etc/pgbackrest \
 && chmod 750 /var/log/pgbackrest /var/spool/pgbackrest /var/lib/pgbackrest
COPY --from=build /build/src/pgbackrest /usr/bin/pgbackrest
COPY deploy/pgbackrest/nexus-pitr-scheduler.sh /usr/local/bin/nexus-pitr-scheduler
COPY deploy/pgbackrest/nexus-pitr-certs.sh /usr/local/bin/nexus-pitr-certs
RUN chmod 755 /usr/local/bin/nexus-pitr-scheduler /usr/local/bin/nexus-pitr-certs \
 && pgbackrest version
