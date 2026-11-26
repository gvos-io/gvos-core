#!/bin/sh
# gvos-kamailio entrypoint: materialize the per-node identity and the
# NATS dial list into the config before exec. The nats module's
# queue-group subjects and the bill-lock reply subject/event-route
# must be unique per node (broadcast semantics + node-targeted RPC
# replies); the cfg ships a @NODE@ placeholder because Kamailio has
# no env interpolation in modparam values or event_route names. The
# @NATS_URL@ placeholder carries the comma-separated broker list for
# the same reason.
set -e

CFG_SRC="${GVOS_CFG_SRC:-/etc/kamailio/kamailio.cfg}"
CFG_RUN=/tmp/kamailio-run.cfg

NODE="${GVOS_NODE_HOST:-kamailio-$(hostname)}"
# NATS queue-group / subject charset: alphanumerics, dashes, dots —
# collapse anything else to a dash.
NODE_SAFE=$(printf '%s' "$NODE" | tr -c 'a-zA-Z0-9.-' '-')

# Broker dial list: the Kamailio nats module passes ONE server string
# to libnats (natsOptions_SetServers, no comma splitting — verified:
# a comma list is treated as a single unresolvable host). On a
# cluster, dial the shared network alias instead: any member is a
# full-topology entry point (the route mesh fans messages out), and
# DNS round-robins the alias across members. GVOS_NATS_URL is still
# the source of truth: its FIRST host is used for the edge dial.
NATS_URL="${GVOS_NATS_URL:-nats://nats:4222}"
# Credentials ride INSIDE the URL userinfo (libnats parses
# user:pass@host off natsOptions_SetServers — the module has no
# separate user/password modparams in this build: nats_user was a
# phantom that aborted startup). Strip them only for the URL-less
# placeholder replacement below when absent.
NATS_USER="${GVOS_NATS_USER:-}"
NATS_PASS="${GVOS_NATS_PASSWORD:-}"
NATS_FIRST=$(printf '%s' "$NATS_URL" | cut -d, -f1 | sed -E 's|^[a-zA-Z0-9+.-]+://||')
if [ -n "$NATS_USER" ] && [ -n "$NATS_PASS" ]; then
    # Re-inject userinfo when the compose URL carried it stripped or
    # when only the env vars are set.
    case "$NATS_FIRST" in
        *@*) ;;                       # userinfo already present
        *) NATS_FIRST="$NATS_USER:$NATS_PASS@$NATS_FIRST" ;;
    esac
fi

# Media isolation (bench): compose passes GVOS_MEDIA_OFF as
# "${GVOS_BENCH_MEDIA_OFF:-}" — an EMPTY string when the host var is
# unset. #!trydefenv in the cfg defines the macro on VAR EXISTENCE,
# not truthiness, so the empty value silently defined GVOS_MEDIA_OFF
# and disabled the rtpengine media path everywhere the variable was
# passed at all (measured: zero offer/answer commands reaching
# rtpengine on a stack where every call succeeded — media was never
# anchored). Unset it here unless it carries a non-empty value.
if [ -z "${GVOS_MEDIA_OFF:-}" ]; then
    unset GVOS_MEDIA_OFF
fi

sed -e "s/@NODE@/$NODE_SAFE/g" -e "s|@NATS_URL@|$NATS_FIRST|g" "$CFG_SRC" > "$CFG_RUN"

exec kamailio -DD -E -f "$CFG_RUN" "$@"
