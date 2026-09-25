#!/bin/sh
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# nexus-nats-auth.sh -- NKey identities and the least-privilege authorization
# config for the Nexus NATS broker (docs/OPERATIONS.md §11, ADR-0003).
#
# One tool for every environment without Helm: the local compose stack (run by
# its `nats-init` container), the Portainer-managed Docker host, and an admin
# workstation preparing keys for Kubernetes (`helm-values`). The Helm chart
# renders the same permission model from values (templates/nats-config.yaml);
# tests/nats/run-integration.sh runs the broker with BOTH renderings and checks
# every role against them.
#
# POSIX sh rather than bash because `compose-init` runs inside the nats-box
# container, which has no bash.
#
# ─── The model ────────────────────────────────────────────────────────────
#
#   account NEXUS (JetStream on)
#     event-service      publish nexus.run.> and nexus.task.dispatch.>, the
#                        JetStream API for its two streams (INFO, CREATE), and
#                        subscribe nexus.run.> plus its own reply inbox.
#     agent-<ns>         subscribe nexus.task.dispatch.<ns>.> ONLY. Publish:
#                        nothing at all. One identity per tenant namespace.
#   account SYS (system account)
#     monitor            read-only $SYS server/account queries, for
#                        `nats server report`, nats-surveyor and the like.
#
# No user is defined outside an account and there is no no_auth_user, so an
# unauthenticated connection is rejected.
#
# ─── Layout of the key directory (NEXUS_NATS_DIR, default ./.dev-nats) ───
#
#   <role>/user.nk        the NKey SEED (secret, 0600). Only that role gets it.
#   <role>/user.pub       its public key (not secret; this is what the server lists)
#   <role>/retiring.pub   previous public key, still accepted during a rotation
#   server/auth.conf      rendered authorization: public keys and permissions only
#   server/nats.conf      rendered server config that includes auth.conf
#
# .dev-nats/ is gitignored. Nothing this tool writes is ever committed.
#
# Commands (run with no arguments for usage):
#   init [--tenant NS]...     keys for event-service, monitor and agent-<NS> if missing
#   add-tenant NS             a key for tenant NS's agents
#   rotate ROLE               new key for ROLE; the old public key stays accepted
#   retire ROLE               stop accepting ROLE's previous key
#   render [--tls DIR [--tls-ca FILE]] [--verify-clients]
#                             write server/auth.conf and server/nats.conf
#   install DEST [--owner U:G] [--agent-tenant NS]
#                             copy each role's seed into DEST/<role>/ (a mounted
#                             volume per role) and the server config into DEST/server/
#   compose-init [--tenant NS] [--tls DIR [--tls-ca FILE]] [--verify-clients] [--owner U:G]
#                             init + render + install /out (the compose nats-init step)
#   helm-values               print the public keys as Helm values (nats.auth.*)
#   list                      print every role and its public key(s)

set -eu

DIR="${NEXUS_NATS_DIR:-.dev-nats}"
NATS_BOX_IMAGE="${NATS_BOX_IMAGE:-natsio/nats-box:0.18.0}"

# Subjects. They mirror internal/bus (DispatchSubject, RunEventSubject,
# RunStateSubject, StreamConfigs, InboxPrefixFor); bus_authz_integration_test.go
# fails if either side changes without the other.
SUBJ_RUNS="nexus.run.>"
SUBJ_DISPATCH="nexus.task.dispatch"
STREAMS="NEXUS_RUNS NEXUS_DISPATCH"
EVENT_SERVICE_INBOX="_INBOX.nexus-event-service.>"

die() { echo "nexus-nats-auth: $*" >&2; exit 1; }
say() { echo "nexus-nats-auth: $*" >&2; }

umask 077

# A namespace becomes ONE subject token. bus.Token percent-encodes anything
# outside [A-Za-z0-9_-]; for those names the token is the name itself, which is
# what this tool (and the Helm chart) can render safely. Anything else is
# refused rather than guessed.
check_ns() {
	printf '%s' "$1" | grep -Eq '^[A-Za-z0-9_-]+$' ||
		die "namespace '$1' is not [A-Za-z0-9_-]+; its subject token would be percent-encoded (internal/bus Token) -- configure that tenant by hand"
}

check_role() {
	case "$1" in
	event-service | monitor) ;;
	agent-*) check_ns "${1#agent-}" ;;
	*) die "unknown role '$1' (event-service, monitor, agent-<namespace>)" ;;
	esac
}

# gen_user DIR: a fresh user NKey. The nats CLI generates it; when it is not on
# PATH (a laptop), the pinned nats-box image does, and only its stdout -- the
# seed and public key -- comes back.
gen_user() {
	d="$1"
	mkdir -p "$d"
	chmod 700 "$d"
	if command -v nats >/dev/null 2>&1 && out=$(nats auth nkey gen user --public 2>/dev/null); then
		:
	elif command -v docker >/dev/null 2>&1; then
		out=$(docker run --rm "$NATS_BOX_IMAGE" nats auth nkey gen user --public) ||
			die "could not generate an NKey with $NATS_BOX_IMAGE"
	else
		die "need the nats CLI (https://github.com/nats-io/natscli) or docker to generate NKeys"
	fi
	seed=$(printf '%s\n' "$out" | sed -n 1p)
	pub=$(printf '%s\n' "$out" | sed -n 2p)
	case "$seed" in SU*) ;; *) die "generated seed is not a user seed" ;; esac
	case "$pub" in U*) ;; *) die "generated public key is not a user key" ;; esac
	printf '%s\n' "$seed" >"$d/user.nk.tmp.$$"
	chmod 600 "$d/user.nk.tmp.$$"
	mv "$d/user.nk.tmp.$$" "$d/user.nk"
	printf '%s\n' "$pub" >"$d/user.pub"
	chmod 644 "$d/user.pub"
	unset seed out
}

ensure_role() {
	check_role "$1"
	if [ -s "$DIR/$1/user.nk" ] && [ -s "$DIR/$1/user.pub" ]; then
		return
	fi
	gen_user "$DIR/$1"
	say "created $1 ($(cat "$DIR/$1/user.pub"))"
}

# keys ROLE: the public keys the server accepts for ROLE (current, then retiring).
keys() {
	cat "$DIR/$1/user.pub"
	[ -s "$DIR/$1/retiring.pub" ] && cat "$DIR/$1/retiring.pub"
	return 0
}

tenants() {
	for d in "$DIR"/agent-*/; do
		[ -s "$d/user.pub" ] || continue
		b=$(basename "$d")
		echo "${b#agent-}"
	done
}

q() { printf '"%s"' "$1"; }

render_auth() {
	[ -s "$DIR/event-service/user.pub" ] || die "no event-service key in $DIR; run init first"
	[ -s "$DIR/monitor/user.pub" ] || die "no monitor key in $DIR; run init first"
	[ -n "$(tenants)" ] || die "no agent-<namespace> key in $DIR; run init --tenant NS first"
	mkdir -p "$DIR/server"
	chmod 755 "$DIR/server"
	out="$DIR/server/auth.conf.tmp.$$"
	{
		echo "# GENERATED by deploy/nats/nexus-nats-auth.sh render. Public keys and"
		echo "# permissions only: nothing here is secret. Do not edit; re-render."
		echo "accounts {"
		echo "  NEXUS {"
		echo "    jetstream: enabled"
		echo "    users: ["
		for k in $(keys event-service); do
			echo "      # event-service: outbox relay (JetStream publish) + SSE waker"
			echo "      { nkey: $k"
			echo "        permissions: {"
			printf '          publish: { allow: [%s, %s' "$(q "$SUBJ_RUNS")" "$(q "$SUBJ_DISPATCH.>")"
			for s in $STREAMS; do
				printf ', %s, %s' "$(q "\$JS.API.STREAM.INFO.$s")" "$(q "\$JS.API.STREAM.CREATE.$s")"
			done
			echo "] }"
			echo "          subscribe: { allow: [$(q "$SUBJ_RUNS"), $(q "$EVENT_SERVICE_INBOX")] }"
			echo "        } }"
		done
		for ns in $(tenants); do
			for k in $(keys "agent-$ns"); do
				echo "      # agents of tenant $ns: dispatch hints for $ns only, publish nothing"
				echo "      { nkey: $k"
				echo "        permissions: {"
				echo "          publish: { deny: [\">\"] }"
				echo "          subscribe: { allow: [$(q "$SUBJ_DISPATCH.$ns.>")] }"
				echo "        } }"
			done
		done
		echo "    ]"
		echo "  }"
		echo "  SYS {"
		echo "    users: ["
		for k in $(keys monitor); do
			echo "      # monitor: read-only server and account queries"
			echo "      { nkey: $k"
			echo "        permissions: {"
			printf '          publish: { allow: ['
			sep=""
			for z in STATSZ VARZ SUBSZ CONNZ ROUTEZ GATEWAYZ LEAFZ ACCOUNTZ JSZ HEALTHZ IDZ; do
				printf '%s%s, %s' "$sep" "$(q "\$SYS.REQ.SERVER.PING.$z")" "$(q "\$SYS.REQ.SERVER.*.$z")"
				sep=", "
			done
			printf ', %s' "$(q '$SYS.REQ.SERVER.PING')"
			for z in CONNZ JSZ STATZ SUBSZ INFO LEAFZ CONNS; do
				printf ', %s' "$(q "\$SYS.REQ.ACCOUNT.*.$z")"
			done
			printf ', %s' "$(q '$SYS.REQ.ACCOUNT.PING.STATZ')"
			echo "] }"
			echo "          subscribe: { allow: [$(q '_INBOX.>'), $(q '$SYS.SERVER.>'), $(q '$SYS.ACCOUNT.>')] }"
			echo "        } }"
		done
		echo "    ]"
		echo "  }"
		echo "}"
		echo "system_account: SYS"
	} >"$out"
	chmod 644 "$out"
	mv "$out" "$DIR/server/auth.conf"
}

render_server() {
	tls_dir="$1"
	verify="$2"
	tls_ca="${3:-$tls_dir/ca.crt}"
	out="$DIR/server/nats.conf.tmp.$$"
	{
		echo "# GENERATED by deploy/nats/nexus-nats-auth.sh render."
		echo "server_name: nexus-nats"
		echo "port: 4222"
		echo "# The monitoring endpoint has NO authentication in nats-server, and /connz"
		echo "# lists every client. Loopback only: the healthcheck runs inside the"
		echo "# container; operators use the SYS account's monitor identity instead."
		echo "http: \"127.0.0.1:8222\""
		echo "max_payload: 1MB"
		echo "jetstream {"
		echo "  store_dir: \"/data\""
		echo "}"
		if [ -n "$tls_dir" ]; then
			echo "tls {"
			echo "  cert_file: \"$tls_dir/tls.crt\""
			echo "  key_file: \"$tls_dir/tls.key\""
			echo "  ca_file: \"$tls_ca\""
			[ "$verify" = 1 ] && echo "  verify: true"
			echo "  timeout: 2"
			echo "}"
		fi
		echo "include \"auth.conf\""
	} >"$out"
	chmod 644 "$out"
	mv "$out" "$DIR/server/nats.conf"
}

cmd_init() {
	ts=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--tenant) [ $# -ge 2 ] || die "--tenant needs a namespace"; ts="$ts $2"; shift 2 ;;
		*) die "init: unknown argument $1" ;;
		esac
	done
	[ -n "$ts" ] || ts="${NEXUS_NAMESPACE:-default}"
	mkdir -p "$DIR"
	chmod 700 "$DIR"
	ensure_role event-service
	ensure_role monitor
	for ns in $ts; do
		check_ns "$ns"
		ensure_role "agent-$ns"
	done
}

cmd_render() {
	tls_dir=""
	tls_ca=""
	verify=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--tls) [ $# -ge 2 ] || die "--tls needs a directory"; tls_dir="$2"; shift 2 ;;
		--tls-ca) [ $# -ge 2 ] || die "--tls-ca needs a file"; tls_ca="$2"; shift 2 ;;
		--verify-clients) verify=1; shift ;;
		*) die "render: unknown argument $1" ;;
		esac
	done
	render_auth
	render_server "$tls_dir" "$verify" "$tls_ca"
	say "rendered $DIR/server/nats.conf and auth.conf (reload a running broker: nats-server --signal reload, or SIGHUP)"
}

cmd_rotate() {
	[ $# -eq 1 ] || die "usage: rotate ROLE"
	check_role "$1"
	d="$DIR/$1"
	[ -s "$d/user.pub" ] || die "no key for $1 to rotate"
	[ -s "$d/retiring.pub" ] && die "$1 already has a retiring key; finish that rotation with 'retire $1' first"
	mv "$d/user.pub" "$d/retiring.pub"
	gen_user "$d"
	render_auth
	say "rotated $1: new $(cat "$d/user.pub"), still accepting $(cat "$d/retiring.pub")"
	say "next: reload the broker, give $1 the new seed ($d/user.nk) and restart it, then 'retire $1' and reload again"
}

cmd_retire() {
	[ $# -eq 1 ] || die "usage: retire ROLE"
	check_role "$1"
	[ -s "$DIR/$1/retiring.pub" ] || die "$1 has no retiring key"
	old=$(cat "$DIR/$1/retiring.pub")
	rm -f "$DIR/$1/retiring.pub"
	render_auth
	say "retired $old for $1; reload the broker to stop accepting it"
}

# install SRC=$DIR into DEST. Each DEST/<name> that EXISTS receives only its own
# material, so a service that mounts one volume at DEST/<name> can read nothing
# else. `agent` is an alias for agent-<--agent-tenant>.
cmd_install() {
	[ $# -ge 1 ] || die "usage: install DEST [--owner U:G] [--agent-tenant NS]"
	dest="$1"
	shift
	owner=""
	agent_ns="${NEXUS_NAMESPACE:-default}"
	while [ $# -gt 0 ]; do
		case "$1" in
		--owner) owner="$2"; shift 2 ;;
		--agent-tenant) agent_ns="$2"; shift 2 ;;
		*) die "install: unknown argument $1" ;;
		esac
	done
	[ -d "$dest" ] || die "$dest does not exist"
	for target in "$dest"/*/; do
		[ -d "$target" ] || continue
		name=$(basename "$target")
		case "$name" in
		server)
			[ -s "$DIR/server/auth.conf" ] || die "nothing rendered; run render first"
			for f in nats.conf auth.conf; do
				cp "$DIR/server/$f" "$target/$f.tmp.$$"
				chmod 444 "$target/$f.tmp.$$"
				mv "$target/$f.tmp.$$" "$target/$f"
			done
			continue
			;;
		agent) role="agent-$agent_ns" ;;
		*) role="$name" ;;
		esac
		check_role "$role"
		[ -s "$DIR/$role/user.nk" ] || die "no seed for $role in $DIR"
		cp "$DIR/$role/user.nk" "$target/user.nk.tmp.$$"
		cp "$DIR/$role/user.pub" "$target/user.pub"
		if [ -n "$owner" ]; then
			chown "$owner" "$target" "$target/user.nk.tmp.$$" "$target/user.pub"
		fi
		chmod 400 "$target/user.nk.tmp.$$"
		chmod 444 "$target/user.pub"
		mv "$target/user.nk.tmp.$$" "$target/user.nk"
		say "installed $role into $target"
	done
}

cmd_compose_init() {
	ts=""
	rargs=""
	owner="10001:10001"
	while [ $# -gt 0 ]; do
		case "$1" in
		--tenant) ts="$ts --tenant $2"; agent_ns="$2"; shift 2 ;;
		--tls) rargs="$rargs --tls $2"; shift 2 ;;
		--tls-ca) rargs="$rargs --tls-ca $2"; shift 2 ;;
		--verify-clients) rargs="$rargs --verify-clients"; shift ;;
		--owner) owner="$2"; shift 2 ;;
		*) die "compose-init: unknown argument $1" ;;
		esac
	done
	# shellcheck disable=SC2086
	cmd_init $ts
	# shellcheck disable=SC2086
	cmd_render $rargs
	cmd_install /out --owner "$owner" --agent-tenant "${agent_ns:-${NEXUS_NAMESPACE:-default}}"
}

cmd_helm_values() {
	printf 'nats:\n  auth:\n    eventService:\n      publicKeys: [%s]\n' "$(keys event-service | paste -sd, -)"
	printf '    monitor:\n      publicKeys: [%s]\n' "$(keys monitor | paste -sd, -)"
	printf '    tenants:\n'
	for ns in $(tenants); do
		printf '      - namespace: %s\n        publicKeys: [%s]\n' "$ns" "$(keys "agent-$ns" | paste -sd, -)"
	done
}

cmd_list() {
	for d in "$DIR"/*/; do
		[ -s "$d/user.pub" ] || continue
		r=$(basename "$d")
		printf '%-24s %s' "$r" "$(cat "$d/user.pub")"
		[ -s "$d/retiring.pub" ] && printf '  (retiring: %s)' "$(cat "$d/retiring.pub")"
		echo
	done
}

usage() {
	sed -n '/^# Commands/,/^$/p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

[ $# -ge 1 ] || usage
c="$1"
shift
case "$c" in
init) cmd_init "$@" ;;
add-tenant) [ $# -eq 1 ] || die "usage: add-tenant NS"; cmd_init --tenant "$1" ;;
rotate) cmd_rotate "$@" ;;
retire) cmd_retire "$@" ;;
render) cmd_render "$@" ;;
install) cmd_install "$@" ;;
compose-init) cmd_compose_init "$@" ;;
helm-values) cmd_helm_values ;;
list) cmd_list ;;
*) usage ;;
esac
