#!/bin/sh
# gvos-kamailio entrypoint: materialize the per-node identity into the
# config before exec. The nats module's queue-group subjects and the
# bill-lock reply subject/event-route must be unique per node
# (broadcast semantics + node-targeted RPC replies); the cfg ships a
# @NODE@ placeholder because Kamailio has no env interpolation in
# modparam values or event_route names.
set -e

CFG_SRC="${GVOS_CFG_SRC:-/etc/kamailio/kamailio.cfg}"
CFG_RUN=/tmp/kamailio-run.cfg

NODE="${GVOS_NODE_HOST:-kamailio-$(hostname)}"
# NATS queue-group / subject charset: alphanumerics, dashes, dots —
# collapse anything else to a dash.
NODE_SAFE=$(printf '%s' "$NODE" | tr -c 'a-zA-Z0-9.-' '-')

sed "s/@NODE@/$NODE_SAFE/g" "$CFG_SRC" > "$CFG_RUN"

exec kamailio -DD -E -f "$CFG_RUN" "$@"
