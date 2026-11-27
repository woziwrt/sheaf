#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Build the sheaf and luci-app-sheaf .apk packages (OpenWrt 25.12, ramips/mt7621) on this computer + a Linux build VM:
#   sh build-apk.sh            -> apk/sheaf-<ver>-r1.apk, apk/luci-app-sheaf-<ver>-r1.apk (MIPS boxes, RBM33G)
#   TARGET=x86 sh build-apk.sh -> apk/x86_64/... (x86 boxes, e.g. UP Squared)
#   TARGET=r4 sh build-apk.sh  -> apk/aarch64/... (BPI-R4 hub: an OpenWrt 25.12 build for the BPI-R4, e.g.
#                               github.com/woziwrt/bpi-r4-deploy; SDK from its release-sdk)
#   SHEAF_SRC=/path/sheaf    -> the binary from another sheaf tree (e.g. a tested older version checked out with
#                               git worktree / git archive), with the scripts of this tree; REL as usual
# sheaf is cross-compiled here (static Go, mipsle softfloat); the packages are made in the OpenWrt SDK 25.12.6
# on the build VM (SDK=..., VM=user@host, VMPORT; the login uses sshpass with VMPASS, so sshpass must be installed
# here). Unsigned: install with LuCI (System -> Software -> Upload package) or apk add --allow-untrusted.
set -e
cd "$(dirname "$0")"
VM=${VM:?set VM=user@build-vm}
VMPORT=${VMPORT:-22}
case ${TARGET:-ramips} in
ramips) SDK=${SDK:-sdk/openwrt-sdk-25.12.6-ramips-mt7621_gcc-14.3.0_musl.Linux-x86_64}; GOENV="GOARCH=mipsle GOMIPS=softfloat"; OUT=apk; DEPS=huawei ;;
x86) SDK=${SDK:-sdk-x86/openwrt-sdk-25.12.6-x86-64_gcc-14.3.0_musl.Linux-x86_64}; GOENV="GOARCH=amd64"; OUT=apk/x86_64; DEPS=huawei ;;
r4) SDK=${SDK:-sdk-r4/openwrt-sdk-mediatek-filogic_gcc-14.3.0_musl.Linux-x86_64}; GOENV="GOARCH=arm64"; OUT=apk/aarch64; DEPS=qmi ;;
*) echo "TARGET ramips, x86 or r4" >&2; exit 2 ;;
esac
SSH="sshpass -p ${VMPASS:?set VMPASS} ssh -o ConnectTimeout=8 -p $VMPORT $VM"
rel=${REL:-1} # package release: raise it when only the scripts change (same sheaf version)
SRC=$(cd "${SHEAF_SRC:-../sheaf}" && pwd)
ver=$(sed -n 's/^const version = "v\([0-9]*\)"/\1/p' "$SRC/status.go")
mkdir -p apk/stage/sheaf/files apk/stage/luci-app-sheaf/files   # apk/ is not in git; files are overwritten
cp package/sheaf/Makefile apk/stage/sheaf/
case $DEPS in # runtime dependencies that differ per target (see package/sheaf/Makefile)
huawei) echo 'SHEAF_DEPENDS:=strongswan-mod-wolfssl (>=0), sms-tool (>=0), wwan (>=0), kmod-usb-net-huawei-cdc-ncm (>=0), kmod-usb-net-cdc-ncm (>=0), kmod-usb-net-cdc-ether (>=0), kmod-usb-serial-option (>=0), kmod-usb-serial-wwan (>=0), kmod-usb-acm (>=0), kmod-usb-wdm (>=0)' ;;
qmi) echo 'SHEAF_DEPENDS:=strongswan-mod-openssl (>=0), uqmi (>=0), kmod-usb-net-qmi-wwan (>=0), kmod-usb-wdm (>=0)' ;;
esac > apk/stage/sheaf/deps.mk
cp package/luci-app-sheaf/Makefile apk/stage/luci-app-sheaf/
STAGE=$(pwd)/apk/stage/sheaf/files
(cd "$SRC" && env GOOS=linux $GOENV CGO_ENABLED=0 go build -trimpath -ldflags=-s -o "$STAGE/sheaf" .)
cp service/sheaf-run service/sheaf-role service/sheaf.init service/sheaf.conf firmware/90-sheaf service/www/sheaf.html \
	service/www/sheaf-status apk/stage/sheaf/files/
cp service/luci/luci-app-sheaf.menu.json service/luci/luci-app-sheaf.acl.json service/luci/mode.js \
	apk/stage/luci-app-sheaf/files/
tar -C apk/stage -czf - sheaf luci-app-sheaf | $SSH "set -e; cd $SDK; tar -C package -xzf -;
	[ -f .config ] || make defconfig >/dev/null;
	make package/sheaf/compile package/luci-app-sheaf/compile SHEAF_VERSION=$ver SHEAF_RELEASE=$rel V=s > /tmp/build-apk.log 2>&1 \
		|| { tail -30 /tmp/build-apk.log; exit 1; }"
mkdir -p $OUT
for p in sheaf luci-app-sheaf; do
	f=$($SSH "ls $SDK/bin/packages/*/base/$p-$ver-r$rel.apk")
	$SSH "cat $f" > "$OUT/$(basename "$f")"
done
ls -la $OUT/*-$ver-r$rel.apk
