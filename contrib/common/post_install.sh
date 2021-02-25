#!/usr/bin/env bash
/usr/bin/getent group chproxy || /usr/sbin/groupadd -r chproxy >/dev/null
/usr/bin/getent passwd chproxy || /usr/sbin/useradd -d /tmp -M -s /bin/false --system -g chproxy chproxy >/dev/null
[[ -e /bin/systemctl ]] && /bin/systemctl daemon-reload ||:
