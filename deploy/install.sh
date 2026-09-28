#!/bin/sh
set -eu
# Run from the uploaded release directory as root. No credentials are bundled.
: "${FOUNDER_USER_ID:?Set the verified founder account ID before installation}"
case "$FOUNDER_USER_ID" in *[!0-9]*|'') exit 1 ;; esac
test "$FOUNDER_USER_ID" -gt 0
test -s saotu-osh-linux.next
install -d -m 755 /opt/saotu-osh/app /opt/saotu-osh/acme
install -d -m 700 /etc/saotu-osh
install -d -m 700 -o nobody -g nobody /var/lib/saotu-osh /var/lib/saotu-osh/media
if test -f /opt/saotu-osh/app/saotu-osh-linux; then
 cp -p /opt/saotu-osh/app/saotu-osh-linux "/opt/saotu-osh/app/saotu-osh-linux.prev.$(date +%Y%m%d-%H%M%S)"
fi
install -m 755 saotu-osh-linux.next /opt/saotu-osh/app/saotu-osh-linux
if ! test -f /etc/saotu-osh/app.env; then
 sed "s/FOUNDER_USER_ID=0/FOUNDER_USER_ID=$FOUNDER_USER_ID/" deploy/app.env.example > /etc/saotu-osh/app.env
 chmod 600 /etc/saotu-osh/app.env
fi
install -m 644 deploy/saotu-osh.service /etc/systemd/system/saotu-osh.service
systemctl daemon-reload
systemctl enable saotu-osh
systemctl restart saotu-osh
curl -fsS http://127.0.0.1:18887/api/health
# Nginx and certificate installation are separate, after existing config is inspected.
