#!/usr/bin/env bash
set -euo pipefail

HOSTS_FILE="/etc/hosts"
ENTRY="127.1.0.101 api.anthropic.com"
ADDED=0

DESTDIR=/tmp
go build -o ${DESTDIR}/ccmproxy.exe ./cmd/ccmproxy
sudo setcap 'cap_net_bind_service=+ep' ${DESTDIR}/ccmproxy.exe
/usr/sbin/getcap ${DESTDIR}/ccmproxy.exe

cleanup() {
    if [[ "$ADDED" -eq 1 ]]; then
        echo "Removing: $ENTRY"

        tmp=$(mktemp)
        grep -vFx "$ENTRY" "$HOSTS_FILE" > "$tmp" || true
        sudo sh -c "cat '$tmp' > '$HOSTS_FILE'"
        rm -f "$tmp"
    fi
}

# 어떤 방식으로든 스크립트가 끝나면 cleanup 실행
trap cleanup EXIT

# signal을 받으면 exit → EXIT trap 실행
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# 이미 존재하는 경우 중복 추가하지 않음
if ! grep -Fxq "$ENTRY" "$HOSTS_FILE"; then
    echo "$ENTRY" | sudo tee -a "$HOSTS_FILE" > /dev/null
    ADDED=1
    echo "Added: $ENTRY"
fi

echo "api.anthropic.com is configured."
echo "Press Ctrl+C to stop."

${DESTDIR}/ccmproxy.exe "$@"
