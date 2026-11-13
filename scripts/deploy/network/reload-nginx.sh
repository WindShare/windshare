#!/usr/bin/env sh
set -eu
/usr/sbin/nginx -t
/usr/bin/systemctl reload nginx
