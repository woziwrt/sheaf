# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# OpenWrt package: sheaf (multi-LTE bonding) with its service, status page and first-boot setup.
# Built by ../../build-apk.sh in the OpenWrt SDK; sheaf itself is a static Go binary staged into files/ beforehand.
include $(TOPDIR)/rules.mk

PKG_NAME:=sheaf
PKG_VERSION:=$(SHEAF_VERSION)
PKG_RELEASE:=$(SHEAF_RELEASE)
PKG_LICENSE:=GPL-3.0-or-later
PKG_MAINTAINER:=Petr Wozniak

include $(INCLUDE_DIR)/package.mk

# per target (written by build-apk.sh): the IPsec crypto plugin and the modem drivers - a MIPS box with Huawei
# modems needs others than an R4 hub (openssl, QMI modems of a Pro 8X)
include $(CURDIR)/deps.mk

define Package/sheaf
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Bond several LTE modems into one tunnel to a server
  URL:=https://github.com/woziwrt/sheaf
  # Runtime dependencies only (written into the package, resolved by apk on the box): as DEPENDS they would make
  # the SDK build every kernel module first (~10 min per build). Must be set here: package-defaults.mk clears it.
  EXTRA_DEPENDS:=kmod-tun (>=0), ip-full (>=0), kmod-ipsec4 (>=0), kmod-crypto-sha256 (>=0), strongswan-charon (>=0), \
	  strongswan-swanctl (>=0), strongswan-mod-kernel-netlink (>=0), strongswan-mod-random (>=0), \
	  strongswan-mod-socket-default (>=0), strongswan-mod-vici (>=0), $(SHEAF_DEPENDS)
endef

define Package/sheaf/description
 sheaf spreads one tunnel (shf0) over every LTE modem of the box, repairs losses, keeps the order of each
 flow and encrypts every link with IPsec. The service dials the modems, sets up IKEv2/ESP to the server and
 restarts what fails. Status page: http://<box>/sheaf.html. Keys are not part of the package:
 /etc/sheaf/sheaf.key and /etc/swanctl/conf.d/sheaf-secret.conf.
endef

define Package/sheaf/conffiles
/etc/sheaf/sheaf.conf
endef

# An upgrade restarts the service. OpenWrt's default_postinst only runs "start", which procd ignores while the old
# instance runs with the same command line: the old supervisor (already read by its shell) kept running after
# "apk add" (59-r2, 26 Nov). Detached and a few seconds later: apk finishes first (luci-app-sheaf in the same
# transaction), and the restart survives the shell that ran apk. A fresh install is started by 90-sheaf / default_postinst.
define Package/sheaf/postinst
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
# logs of a night survive: the default 64 kB ring held ~10 min of sheaf's lines (takes effect with the next
# restart of the log service or a boot; not restarted here, the running ring would be lost)
[ "$$(uci -q get system.@system[0].log_size)" -ge 512 ] 2>/dev/null || { uci set system.@system[0].log_size='512'; uci commit system; }
if [ "$${PKG_UPGRADE}" = 1 ] && /etc/init.d/sheaf running >/dev/null 2>&1; then
	echo "sheaf: restarting the service in 5 s (new supervisor); the tunnel is down for about a minute"
	setsid sh -c 'sleep 5; /etc/init.d/sheaf restart' </dev/null >/dev/null 2>&1 &
fi
exit 0
endef

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
endef

define Build/Compile
endef

define Package/sheaf/install
	$(INSTALL_DIR) $(1)/usr/sbin $(1)/etc/init.d $(1)/etc/sheaf $(1)/etc/uci-defaults $(1)/www/cgi-bin
	$(INSTALL_BIN) ./files/sheaf $(1)/usr/sbin/sheaf
	$(INSTALL_BIN) ./files/sheaf-run $(1)/usr/sbin/sheaf-run
	$(INSTALL_BIN) ./files/sheaf-role $(1)/usr/sbin/sheaf-role
	$(INSTALL_BIN) ./files/sheaf.init $(1)/etc/init.d/sheaf
	$(INSTALL_CONF) ./files/sheaf.conf $(1)/etc/sheaf/sheaf.conf
	$(INSTALL_BIN) ./files/90-sheaf $(1)/etc/uci-defaults/90-sheaf
	$(INSTALL_DATA) ./files/sheaf.html $(1)/www/sheaf.html
	$(INSTALL_BIN) ./files/sheaf-status $(1)/www/cgi-bin/sheaf-status
endef

$(eval $(call BuildPackage,sheaf))
