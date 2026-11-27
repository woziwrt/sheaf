# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# OpenWrt package: LuCI page Services -> Sheaf (choice of the bonding mode).
include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-sheaf
PKG_VERSION:=$(SHEAF_VERSION)
PKG_RELEASE:=$(SHEAF_RELEASE)
PKG_LICENSE:=GPL-3.0-or-later
PKG_MAINTAINER:=Petr Wozniak

include $(INCLUDE_DIR)/package.mk

define Package/luci-app-sheaf
  SECTION:=luci
  CATEGORY:=LuCI
  SUBMENU:=3. Applications
  TITLE:=LuCI page for sheaf: bonding mode
  PKGARCH:=all
  # Runtime dependencies only (written into the package, resolved by apk on the box): as DEPENDS they would make
  # the SDK build every kernel module first (~10 min per build). Must be set here: package-defaults.mk clears it.
  EXTRA_DEPENDS:=sheaf (>=0), luci-base (>=0), rpcd-mod-file (>=0)
endef

define Package/luci-app-sheaf/description
 Services -> Sheaf: speed / smooth / redundant / backup and the primary modem. Writes /etc/sheaf/mode,
 which sheaf reads on change; the server follows the box.
endef

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
endef

define Build/Compile
endef

define Package/luci-app-sheaf/install
	$(INSTALL_DIR) $(1)/usr/share/luci/menu.d $(1)/usr/share/rpcd/acl.d $(1)/www/luci-static/resources/view/sheaf
	$(INSTALL_DATA) ./files/luci-app-sheaf.menu.json $(1)/usr/share/luci/menu.d/luci-app-sheaf.json
	$(INSTALL_DATA) ./files/luci-app-sheaf.acl.json $(1)/usr/share/rpcd/acl.d/luci-app-sheaf.json
	$(INSTALL_DATA) ./files/mode.js $(1)/www/luci-static/resources/view/sheaf/mode.js
endef

define Package/luci-app-sheaf/postinst
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
rm -f /tmp/luci-indexcache*
/etc/init.d/rpcd reload
exit 0
endef

$(eval $(call BuildPackage,luci-app-sheaf))
