#!/bin/bash

NAME=chproxy

die() {
    if [[ $1 -eq 0 ]]; then
        rm -rf "${TMPDIR}"
    else
        [ "${TMPDIR}" = "" ] || echo "Temporary data stored at '${TMPDIR}'"
    fi
    echo "$2"
    exit $1
}

pwd

GIT_VERSION="$(git describe --always --tags | sed -e 's/^v//')" || die 1 "Can't get latest version from git"

set -f; IFS='-' ; arr=($GIT_VERSION)
VERSION=${arr[0]}; [ -z "${arr[2]}" ] && RELEASE=${arr[1]} || RELEASE=${arr[1]}.${arr[2]}
set +f; unset IFS

[ "${VERSION}" = "" -o  "${RELEASE}" = "" ] && {
    echo "Revision: ${RELEASE}";
    echo "Version: ${VERSION}";
    echo
    echo "Known tags:"
    git tag
    echo;
    die 1 "Can't parse version from git";
}

printf "'%s' '%s'\n" "$VERSION" "$RELEASE"

make release-build || exit 1

TMPDIR=$(mktemp -d)
[ "${TMPDIR}" = "" ] && die 1 "Can't create temp dir"
echo version ${VERSION} release ${RELEASE}
mkdir -p "${TMPDIR}/usr/bin" || die 1 "Can't create bin dir"
mkdir -p "${TMPDIR}/usr/share/doc/${NAME}" || die 1 "Can't create share dir"
mkdir -p "${TMPDIR}/usr/lib/systemd/system" || die 1 "Can't create systemd dir"
cp -r ./${NAME} "${TMPDIR}/usr/bin/" || die 1 "Can't install package binary"
cp -r ./config/examples "${TMPDIR}/usr/share/doc/${NAME}" || die 1 "Can't install package shared files"
# Deb Specific
mkdir -p "${TMPDIR}/etc/default" || die 1 "Can't create sysconfig dir"
cp ./contrib/common/${NAME}.env "${TMPDIR}/etc/default/${NAME}" || die 1 "Can't install package sysconfig file"
cp ./contrib/deb/${NAME}.service "${TMPDIR}/usr/lib/systemd/system" || die 1 "Can't install package systemd files"
#

fpm -s dir -t deb -n ${NAME} -v ${VERSION} -C ${TMPDIR} \
    --iteration ${RELEASE} \
    -p ${NAME}-VERSION-ITERATION.ARCH.deb \
    --description "chproxy: ClickHouse http proxy and load balancer" \
    --license BSD-2 \
    --url "https://github.com/Vertamedia/chproxy" \
    --after-install contrib/common/post_install.sh \
    "${@}" \
    etc usr/bin usr/lib/systemd usr/share || die 1 "Can't create package!"

die 0 "Success"
