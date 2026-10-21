#!/bin/sh
# Lets the probe read the certificate certbot already issued, without copying
# the key anywhere. certbot keeps /etc/letsencrypt root-only by default.
#
# Run once as root, after reading it. It creates the group "ondrya-cert" and
# makes the certbot tree readable by that group; certbot keeps those
# permissions on renewal. The deploy hook it installs repeats the step for new
# certificates, so the probe never needs a restart or a copy of the key.
#
#   sh deploy/grant-cert-access.sh [letsencrypt-dir]
set -eu

DIR=${1:-/etc/letsencrypt}
GROUP=ondrya-cert
HOOK="$DIR/renewal-hooks/deploy/ondrya-probe-perms.sh"

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
[ -d "$DIR/live" ] || { echo "$DIR/live not found: is certbot installed and has it issued a certificate?" >&2; exit 1; }

getent group "$GROUP" >/dev/null || groupadd --system "$GROUP"

apply() {
  chgrp "$GROUP" "$DIR" "$DIR/live" "$DIR/archive"
  chmod 0750 "$DIR" "$DIR/live" "$DIR/archive"
  for d in "$DIR"/live/*/ "$DIR"/archive/*/; do
    [ -d "$d" ] || continue
    chgrp "$GROUP" "$d"
    chmod 0750 "$d"
  done
  for k in "$DIR"/archive/*/privkey*.pem; do
    [ -e "$k" ] || continue
    chgrp "$GROUP" "$k"
    chmod 0640 "$k"
  done
}
apply

mkdir -p "$(dirname "$HOOK")"
cat >"$HOOK" <<HOOKEOF
#!/bin/sh
# Installed by ondrya-probe grant-cert-access.sh: new key files stay readable by $GROUP.
set -eu
DIR=$DIR
GROUP=$GROUP
$(sed -n '/^apply() {/,/^}/p' "$0")
apply
HOOKEOF
chmod 0755 "$HOOK"

echo "group $GROUP (gid $(getent group "$GROUP" | cut -d: -f3)) can now read $DIR"
echo "docker: put that gid in PROBE_CERT_GID; systemd: the unit already uses the group name"
