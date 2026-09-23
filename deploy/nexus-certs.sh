#!/bin/sh
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# nexus-certs.sh -- issue, rotate, install and check the certificates for
# Nexus inter-service gRPC (docs/TLS.md).
#
# One tool for the two environments that have no cert-manager: the local
# compose stack (`make dev-certs`) and the Portainer-managed Docker host. AKS
# uses cert-manager instead (deploy/k8s/cert-*.yaml), but lays files out the
# same way -- <dir>/tls.crt, tls.key, ca.crt -- so the services' environment is
# identical everywhere and only the issuer differs.
#
# POSIX sh rather than bash because `install` runs inside an alpine init
# container (deploy/docker-compose.tls.yml), which has no bash.
#
# Layout of OUT (default ./.dev-certs, which is gitignored):
#
#   ca/ca.crt, ca/ca.key         the CURRENT issuing CA. ca.key never leaves the
#                                machine that issues -- `install` refuses to copy it.
#   ca-retired-<stamp>/ca.crt    previous CAs, kept only to build the overlap bundle
#   ca-bundle.pem                every CA peers should trust right now
#   <name>/tls.crt, tls.key      one leaf per service or agent
#   <name>/ca.crt                copy of ca-bundle.pem at issue time (k8s layout)
#
# Commands (run with no arguments for usage):
#   dev                  CA if missing + a leaf for every compose service and agent
#   init-ca              create the CA (refuses to overwrite one)
#   issue NAME ...       issue or renew one leaf
#   rotate-ca            new CA; bundle = new + old (overlap), old key destroyed
#   drop-retired-cas     bundle = current CA only (finish a CA rotation)
#   install SRC DEST     copy leaves + bundle (never CA keys) into DEST atomically
#   check [--warn-days N]  print expiry of everything; exit 1 if any is near
#
# Nothing here is a secret-management system. It is the smallest honest tool
# for issuing short-lived leaves from a CA you control; see docs/TLS.md §5 for
# where each environment's CA key should live.

set -eu

OUT="${NEXUS_CERT_DIR:-.dev-certs}"
LEAF_DAYS="${NEXUS_CERT_DAYS:-30}"
CA_DAYS="${NEXUS_CA_DAYS:-365}"
CA_NAME="${NEXUS_CA_NAME:-Nexus Dev Internal CA}"
OPENSSL="${OPENSSL:-openssl}"

# Every compose service that serves gRPC, and the agents that dial it. The
# SANs ARE these names: config.PeerTarget dials dns:///<service>:port and gRPC
# verifies the certificate against <service> (docs/TLS.md §4.3).
DEV_SERVICES="api-gateway workflow-manager dag-engine scheduler scheduler-gpu resource-manager event-service"
DEV_AGENTS="agent-cpu agent-gpu agent-gpu-a10"

die() { echo "nexus-certs: $*" >&2; exit 1; }
say() { echo "nexus-certs: $*"; }

need_openssl() {
	command -v "$OPENSSL" >/dev/null 2>&1 || die "openssl not found (set OPENSSL=/path/to/openssl)"
}

# Keys are created 0600 inside 0700 directories: umask first, so there is no
# window in which a key exists with looser permissions.
umask 077

serial() {
	# 128 random bits. Serials must be unique per CA, and the reload logs and
	# tests identify a certificate by its serial.
	echo "0x$("$OPENSSL" rand -hex 16)"
}

genkey() { # out-path
	tmp="$1.sec1.$$"
	"$OPENSSL" ecparam -name prime256v1 -genkey -noout -out "$tmp"
	"$OPENSSL" pkcs8 -topk8 -nocrypt -in "$tmp" -out "$1"
	rm -f "$tmp"
	chmod 600 "$1"
}

stamp() { date -u +%Y%m%dT%H%M%SZ; }

init_ca() {
	need_openssl
	[ -e "$OUT/ca/ca.key" ] && die "a CA already exists in $OUT/ca; use rotate-ca to replace it"
	mkdir -p "$OUT/ca"
	chmod 700 "$OUT" "$OUT/ca"
	genkey "$OUT/ca/ca.key"
	cfg="$OUT/ca/.openssl.cnf"
	cat >"$cfg" <<EOF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = $CA_NAME $(stamp)
[v3_ca]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
EOF
	"$OPENSSL" req -x509 -new -key "$OUT/ca/ca.key" -config "$cfg" -extensions v3_ca \
		-days "$CA_DAYS" -set_serial "$(serial)" -out "$OUT/ca/ca.crt"
	rm -f "$cfg"
	chmod 644 "$OUT/ca/ca.crt"
	build_bundle
	say "created CA $OUT/ca/ca.crt (valid $CA_DAYS days). Its key is $OUT/ca/ca.key: keep it off every machine it issues for."
}

# The bundle is the current CA plus any retired CA that is still valid. That is
# what makes a CA rotation hitless: peers trust both while leaves move over.
build_bundle() {
	tmp="$OUT/ca-bundle.pem.tmp.$$"
	cat "$OUT/ca/ca.crt" >"$tmp"
	for old in "$OUT"/ca-retired-*/ca.crt; do
		[ -f "$old" ] || continue
		if "$OPENSSL" x509 -in "$old" -noout -checkend 0 >/dev/null 2>&1; then
			cat "$old" >>"$tmp"
		fi
	done
	chmod 644 "$tmp"
	mv -f "$tmp" "$OUT/ca-bundle.pem"
}

# issue NAME [--dns a,b] [--ip x,y] [--client-only] [--days N]
issue() {
	need_openssl
	[ $# -ge 1 ] || die "usage: issue NAME [--dns a,b] [--ip x,y] [--client-only] [--days N]"
	name="$1"; shift
	dns="$name"; ips=""; eku="serverAuth, clientAuth"; days="$LEAF_DAYS"
	while [ $# -gt 0 ]; do
		case "$1" in
		--dns) dns="$2"; shift 2 ;;
		--ip) ips="$2"; shift 2 ;;
		--client-only) eku="clientAuth"; shift ;;
		--days) days="$2"; shift 2 ;;
		*) die "issue: unknown option $1" ;;
		esac
	done
	[ -f "$OUT/ca/ca.key" ] || die "no CA in $OUT/ca (run init-ca, or point NEXUS_CERT_DIR at the directory holding it)"

	dir="$OUT/$name"
	mkdir -p "$dir"
	chmod 700 "$dir"
	work="$dir/.issue.$$"
	mkdir -p "$work"

	san=""
	if [ "$eku" != "clientAuth" ]; then
		for d in $(echo "$dns" | tr ',' ' '); do san="${san:+$san, }DNS:$d"; done
		for i in $(echo "$ips" | tr ',' ' '); do san="${san:+$san, }IP:$i"; done
	fi
	{
		echo "[req]"
		echo "distinguished_name = dn"
		echo "prompt = no"
		echo "[dn]"
		echo "CN = $name"
		echo "[leaf]"
		echo "basicConstraints = critical, CA:FALSE"
		echo "keyUsage = critical, digitalSignature"
		echo "extendedKeyUsage = $eku"
		echo "authorityKeyIdentifier = keyid"
		[ -n "$san" ] && echo "subjectAltName = $san"
	} >"$work/openssl.cnf"

	# New key per issuance. Renewing with the old key would make a leaked key
	# outlive the certificate it was stolen with.
	genkey "$work/tls.key"
	"$OPENSSL" req -new -key "$work/tls.key" -config "$work/openssl.cnf" -out "$work/tls.csr"
	"$OPENSSL" x509 -req -in "$work/tls.csr" -CA "$OUT/ca/ca.crt" -CAkey "$OUT/ca/ca.key" \
		-set_serial "$(serial)" -days "$days" -extfile "$work/openssl.cnf" -extensions leaf \
		-out "$work/tls.crt" 2>/dev/null
	chmod 644 "$work/tls.crt"
	cp "$OUT/ca-bundle.pem" "$work/ca.crt"
	chmod 644 "$work/ca.crt"

	# Key first, then certificate, each by rename: a reader never sees a torn
	# file. The pair is still briefly mismatched between the two renames; the
	# services' reloader debounces and validates, so that window is harmless.
	mv -f "$work/tls.key" "$dir/tls.key"
	mv -f "$work/tls.crt" "$dir/tls.crt"
	mv -f "$work/ca.crt" "$dir/ca.crt"
	rm -rf "$work"
	say "issued $dir/tls.crt ($eku; ${san:-no SAN}; $days days; serial $("$OPENSSL" x509 -in "$dir/tls.crt" -noout -serial | cut -d= -f2))"
}

dev() {
	[ -f "$OUT/ca/ca.key" ] || init_ca
	for s in $DEV_SERVICES; do
		# localhost/127.0.0.1 too: the gateway's worker port is published to the
		# host, so an agent or grpcurl on the laptop dials it by those names.
		issue "$s" --dns "$s,localhost" --ip 127.0.0.1
	done
	for a in $DEV_AGENTS; do
		issue "$a" --client-only
	done
	say "done. Start the TLS stack with: make up-tls   (or see docs/TLS.md §5.1)"
}

rotate_ca() {
	need_openssl
	[ -f "$OUT/ca/ca.key" ] || die "no CA to rotate in $OUT/ca"
	retired="$OUT/ca-retired-$(stamp)"
	mv "$OUT/ca" "$retired"
	# The retired CA's KEY is destroyed: it will never sign again, and a key
	# that is kept "just in case" is a key that can be stolen.
	rm -f "$retired/ca.key"
	init_ca
	say "CA rotated. $OUT/ca-bundle.pem now trusts the new AND the retired CA."
	say "next: distribute the bundle (install), re-issue every leaf, then drop-retired-cas and distribute again."
}

drop_retired() {
	need_openssl
	# Refuse while any leaf here was signed by a retired CA. Dropping the old CA
	# from the bundle first is precisely the outage the overlap exists to avoid:
	# every peer still holding an old-CA leaf is rejected at its next handshake.
	stale=""
	for d in "$OUT"/*/; do
		[ -f "$d/tls.crt" ] || continue
		"$OPENSSL" verify -CAfile "$OUT/ca/ca.crt" "$d/tls.crt" >/dev/null 2>&1 || stale="$stale $(basename "$d")"
	done
	if [ -n "$stale" ] && [ "${1:-}" != "--force" ]; then
		die "these leaves are not signed by the current CA; re-issue them first (or pass --force):$stale"
	fi
	rm -rf "$OUT"/ca-retired-*
	build_bundle
	for d in "$OUT"/*/; do
		[ -f "$d/tls.crt" ] || continue
		cp "$OUT/ca-bundle.pem" "$d/ca.crt.tmp"
		mv -f "$d/ca.crt.tmp" "$d/ca.crt"
	done
	say "bundle now trusts only the current CA. Distribute it (install) to finish the rotation."
}

# install SRC DEST [--owner uid:gid]
#
# Copies every leaf directory and the bundle from SRC into DEST, NEVER a CA key.
# Each file lands by rename, key before certificate, so a running service's
# reloader sees either the old pair or the new one (plus, at worst, a brief
# mismatch it debounces). Runs without openssl, so it works in a bare alpine.
install_certs() {
	[ $# -ge 2 ] || die "usage: install SRC DEST [--owner uid:gid]"
	src="$1"; dest="$2"; shift 2
	owner=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--owner) owner="$2"; shift 2 ;;
		*) die "install: unknown option $1" ;;
		esac
	done
	[ -f "$src/ca-bundle.pem" ] || die "install: $src/ca-bundle.pem not found"
	mkdir -p "$dest"
	put() { # src dest mode
		cp "$1" "$2.tmp.$$"
		chmod "$3" "$2.tmp.$$"
		[ -n "$owner" ] && chown "$owner" "$2.tmp.$$"
		mv -f "$2.tmp.$$" "$2"
	}
	put "$src/ca-bundle.pem" "$dest/ca-bundle.pem" 444
	n=0
	for d in "$src"/*/; do
		name=$(basename "$d")
		case "$name" in ca|ca-retired-*) continue ;; esac
		[ -f "$d/tls.crt" ] && [ -f "$d/tls.key" ] || continue
		mkdir -p "$dest/$name"
		chmod 755 "$dest/$name"
		# 0400 and owned by the service user: the key is readable by the
		# process that needs it and nobody else in the container.
		put "$d/tls.key" "$dest/$name/tls.key" 400
		put "$d/tls.crt" "$dest/$name/tls.crt" 444
		put "$src/ca-bundle.pem" "$dest/$name/ca.crt" 444
		n=$((n + 1))
	done
	say "installed $n leaf pair(s) and the CA bundle into $dest"
}

# check [--warn-days N] [DIR]
check() {
	need_openssl
	warn=7
	while [ $# -gt 0 ]; do
		case "$1" in
		--warn-days) warn="$2"; shift 2 ;;
		*) OUT="$1"; shift ;;
		esac
	done
	rc=0
	for f in "$OUT"/ca/ca.crt "$OUT"/*/tls.crt; do
		[ -f "$f" ] || continue
		end=$("$OPENSSL" x509 -in "$f" -noout -enddate | cut -d= -f2)
		if ! "$OPENSSL" x509 -in "$f" -noout -checkend 0 >/dev/null 2>&1; then
			state="EXPIRED"; rc=1
		elif ! "$OPENSSL" x509 -in "$f" -noout -checkend $((warn * 86400)) >/dev/null 2>&1; then
			state="RENEW NOW (<${warn}d)"; rc=1
		else
			state="ok"
		fi
		printf '%-50s %-28s %s\n' "$f" "$end" "$state"
	done
	return $rc
}

usage() {
	sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
}

cmd="${1:-}"
[ -n "$cmd" ] || usage
shift
case "$cmd" in
dev) dev ;;
init-ca) init_ca ;;
issue) issue "$@" ;;
rotate-ca) rotate_ca ;;
drop-retired-cas) drop_retired "$@" ;;
install) install_certs "$@" ;;
check) check "$@" ;;
-h | --help | help) usage ;;
*) die "unknown command $cmd (try --help)" ;;
esac
