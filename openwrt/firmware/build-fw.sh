#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Build the sheaf firmware for MikroTik RBM33G with the OpenWrt 25.12.6 ImageBuilder (on a Linux build machine).
#   sh build-fw.sh <overlay dir> <dir with sheaf-*.apk and luci-app-sheaf-*.apk>
# sheaf comes as its own packages (../build-apk.sh): the image's sheaf is then owned by apk and a later
# "apk add sheaf-<new>.apk" upgrades it cleanly. The ImageBuilder signs its local package index with its own build
# key, so the official packages stay signature-checked. The overlay holds only the fixed EIP93 module
# (one-line fix, openwrt/openwrt#26910): it replaces the packaged crypto-hw-eip93.ko and belongs to this kernel.
set -e
IB=${IB:-$HOME/sheaf-fw/openwrt-imagebuilder-25.12.6-ramips-mt7621.Linux-x86_64}
FILES=$(cd "$1" && pwd)
cp "$2"/sheaf-*.apk "$2"/luci-app-sheaf-*.apk "$IB/packages/"
PKGS="luci ip-full kmod-tun kmod-ipsec4 kmod-crypto-hw-eip93 \
 strongswan-charon strongswan-swanctl strongswan-mod-kernel-netlink strongswan-mod-random strongswan-mod-socket-default \
 strongswan-mod-vici strongswan-mod-wolfssl \
 sms-tool picocom chat wwan kmod-usb-net-huawei-cdc-ncm kmod-usb-net-cdc-ncm kmod-usb-net-cdc-ether \
 kmod-usb-serial-option kmod-usb-serial-wwan kmod-usb-acm kmod-usb-wdm rpcd-mod-file \
 sheaf luci-app-sheaf"
make -C "$IB" image PROFILE=mikrotik_routerboard-m33g PACKAGES="$PKGS" FILES="$FILES" EXTRA_IMAGE_NAME=sheaf
ls -la "$IB"/bin/targets/ramips/mt7621/*sheaf*
