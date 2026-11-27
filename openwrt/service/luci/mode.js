// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak
'use strict';
'require view';
'require fs';
'require ui';

// Sheaf mode for LuCI: writes "mode primary" to /etc/sheaf/mode; sheaf reads the file when it changes and sends
// the choice to the server with its feedback.
var modes = [
	['speed', 'Speed', 'both modems at once, losses are repaired – internet, downloads (default)'],
	['smooth', 'Smooth', 'UDP (calls, live video) does not wait for a lost packet – cameras, video streaming'],
	['redundant', 'Redundant', 'every packet over both modems – a modem outage goes unnoticed; 2× the data'],
	['backup', 'Backup', 'primary modem only, the other one only when it fails – saves data']
];
// sheaf numbers its links 1, 2, ... in the order sheaf-run passed them (/tmp/sheaf-links.ids, IKE ids): with one
// modem missing, link 1 is the other modem - the labels follow that file, never a fixed order
var names = { o2: 'O2', vf: 'Vodafone', tm: 'T-Mobile' };
function linkList(ids) {
	var l = (ids || '').trim().split(/\s+/).filter(function(x) { return x; });
	if (!l.length) l = ['o2', 'vf']; // sheaf not running: the box's two modems
	return l.map(function(id, i) { return [String(i + 1), (names[id] || id) + ' (L' + (i + 1) + ')']; });
}

return view.extend({
	load: function() {
		return Promise.all([
			L.resolveDefault(fs.read('/etc/sheaf/mode'), ''),
			L.resolveDefault(fs.read('/tmp/sheaf-status.json'), ''),
			L.resolveDefault(fs.read('/tmp/sheaf-links.ids'), '')
		]);
	},

	render: function(data) {
		var f = (data[0] || '').trim().split(/\s+/), st = {};
		try { st = JSON.parse(data[1] || '{}'); } catch (e) {}
		var cur = f[0] || 'speed', prim = f[1] || '2', links = linkList(data[2]);

		var radios = modes.map(function(m) {
			return E('label', { 'style': 'display:block;margin:.4em 0' }, [
				E('input', { 'type': 'radio', 'name': 'sheafmode', 'value': m[0], 'checked': m[0] == cur ? '' : null }),
				' ', E('strong', {}, m[1]), ' – ', m[2]
			]);
		});
		var sel = E('select', { 'id': 'sheafprim', 'class': 'cbi-input-select' }, links.map(function(l) {
			return E('option', { 'value': l[0], 'selected': l[0] == prim ? '' : null }, l[1]);
		}));

		return E('div', { 'class': 'cbi-map' }, [
			E('h2', {}, 'Sheaf – mode'),
			E('p', {}, [ 'Running now: ', E('strong', {}, st.mode || '?'),
				st.mode == 'backup' ? ' (primary ' + ((links[st.primary - 1] || [])[1] || 'L' + st.primary) + ')' : '', ' · ',
				E('a', { 'href': '/sheaf.html' }, 'box status') ]),
			E('div', { 'class': 'cbi-section' }, [
				E('div', {}, radios),
				E('p', {}, [ 'Primary modem for Backup: ', sel ]),
				E('p', { 'class': 'cbi-section-descr' },
					'Takes effect at once, no restart; the server follows the box\'s mode. Small important packets (DNS, connection setup) always go over both modems.')
			])
		]);
	},

	handleSave: function() {
		var m = document.querySelector('input[name="sheafmode"]:checked');
		var p = document.getElementById('sheafprim').value;
		if (!m)
			return;
		return fs.write('/etc/sheaf/mode', m.value + ' ' + p + '\n').then(function() {
			ui.addNotification(null, E('p', {}, 'Mode saved: ' + m.value), 'info');
		}).catch(function(e) {
			ui.addNotification(null, E('p', {}, 'Saving failed: ' + e.message), 'error');
		});
	},

	handleSaveApply: null,
	handleReset: null
});
