// Shared by the host page and the player: turns the flat playlist into folder headers + indented items.
const PL = {
  collapsed: new Set(), seen: new Set(), lastCur: null,
  prefixes(g, incl = true) {
    const out = []; let p = '';
    g.split('/').forEach((s, k) => { p = k ? p + '/' + s : s; out.push(p); });
    return incl ? out : out.slice(0, -1);
  },
  inside: (it, p) => it.group === p || it.group.startsWith(p + '/'),
  // Big folders start collapsed (except the one holding what's playing); everything else is open.
  init(items, cur) {
    const curGroup = (items.find(i => i.id === cur) || {}).group || '';
    for (const g of new Set(items.map(i => i.group).filter(Boolean))) {
      for (const p of this.prefixes(g)) {
        if (this.seen.has(p)) continue;
        this.seen.add(p);
        if (!(curGroup === p || curGroup.startsWith(p + '/')) && items.filter(i => this.inside(i, p)).length > 15) this.collapsed.add(p);
      }
    }
    if (cur !== this.lastCur) { this.lastCur = cur; for (const p of curGroup ? this.prefixes(curGroup) : []) this.collapsed.delete(p); }
  },
  rows(items) {
    const rows = [], emitted = new Set();
    for (const it of items) {
      if (it.group) {
        for (const p of this.prefixes(it.group)) {
          if (emitted.has(p)) continue; emitted.add(p);
          if (!this.prefixes(p, false).some(a => this.collapsed.has(a)))
            rows.push({ hdr: true, path: p, label: p.split('/').pop(), depth: p.split('/').length - 1,
                        count: items.filter(i => this.inside(i, p)).length, collapsed: this.collapsed.has(p) });
        }
        if (this.prefixes(it.group).some(a => this.collapsed.has(a))) continue;
      }
      rows.push({ item: it, depth: it.group ? it.group.split('/').length : 0 });
    }
    return rows;
  },
  toggle(p) { this.collapsed.has(p) ? this.collapsed.delete(p) : this.collapsed.add(p); },
  pretty: g => g.replace(/\//g, ' › '),
};
