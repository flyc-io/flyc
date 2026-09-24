/* Flyc — interface d'administration. Vanilla JS, routage par ancre, consomme l'API v1. */
(function () {
  'use strict';
  const $app = document.getElementById('app');
  const state = { account: null, tenant: localStorage.getItem('flyc.tenant') || '', tenants: [], authConfig: {} };

  // ---------- utilitaires ----------
  function h(tag, attrs, ...children) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === 'class') el.className = v;
      else if (k === 'html') el.innerHTML = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else if (v !== null && v !== undefined && v !== false) el.setAttribute(k, v === true ? '' : v);
    }
    for (const c of children.flat(Infinity)) if (c !== null && c !== undefined && c !== false) el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    return el;
  }
  function toast(msg, bad) {
    const t = h('div', { class: 'toast' + (bad ? ' bad' : '') }, msg);
    document.getElementById('toasts').append(t);
    setTimeout(() => t.remove(), bad ? 6000 : 3000);
  }
  async function api(method, path, body, opts) {
    const headers = { 'X-Requested-With': 'flyc' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (state.tenant && !(opts && opts.noTenant)) headers['X-Flyc-Tenant'] = state.tenant;
    const r = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store' });
    if (r.status === 204) return null;
    let j = null;
    try { j = await r.json(); } catch (e) { /* vide */ }
    if (r.status === 401 && !path.startsWith('/auth/')) { state.account = null; location.hash = '#/login'; throw new Error(j && j.message || 'session expirée'); }
    if (!r.ok) throw Object.assign(new Error(j && j.message || ('erreur ' + r.status)), { status: r.status, body: j });
    return j;
  }
  const fmtDate = (s) => s ? new Date(s).toLocaleString('fr-FR') : '–';
  const fmtN = (n) => Number(n || 0).toLocaleString('fr-FR');
  const pill = (cls, txt) => h('span', { class: 'pill ' + cls }, txt);
  const statePill = (s) => pill({ open: 'ok', queue: 'wait', closed: 'bad' }[s] || 'muted', s);
  function confirmDialog(title, text, onOk, danger) {
    const d = h('dialog', {}, h('div', { class: 'body' }, h('h2', {}, title), h('p', {}, text),
      h('div', { class: 'row' }, h('button', { class: 'btn secondary', onclick: () => d.close() }, 'Annuler'),
        h('button', { class: 'btn' + (danger ? ' danger' : ''), onclick: async () => { try { await onOk(); d.close(); } catch (e) { toast(e.message, true); } } }, 'Confirmer'))));
    document.body.append(d); d.showModal(); d.addEventListener('close', () => d.remove());
  }
  function formDialog(title, fields, onSubmit, submitLabel) {
    const inputs = {};
    const form = h('form', { class: 'stack', onsubmit: async (e) => { e.preventDefault(); const v = {}; for (const [k, el] of Object.entries(inputs)) v[k] = el.type === 'checkbox' ? el.checked : el.value; try { await onSubmit(v, d); d.close(); } catch (err) { toast(err.message, true); } } });
    for (const f of fields) {
      let el;
      if (f.type === 'select') el = h('select', { name: f.name }, f.options.map(o => h('option', { value: o[0], selected: o[0] === f.value }, o[1])));
      else if (f.type === 'checkbox') { el = h('input', { type: 'checkbox', name: f.name, checked: !!f.value }); el.checked = !!f.value; }
      else if (f.type === 'textarea') el = h('textarea', { name: f.name, rows: 3 }, f.value || '');
      else { el = h('input', { type: f.type || 'text', name: f.name, placeholder: f.placeholder || '', required: !!f.required, step: f.step, min: f.min }); el.value = f.value ?? ''; }
      inputs[f.name] = el;
      form.append(f.type === 'checkbox' ? h('label', { class: 'row' }, el, ' ', f.label) : h('label', {}, f.label, el, f.hint ? h('span', { class: 'hint' }, f.hint) : null));
    }
    form.append(h('div', { class: 'row' }, h('button', { type: 'button', class: 'btn secondary', onclick: () => d.close() }, 'Annuler'), h('button', { type: 'submit', class: 'btn' }, submitLabel || 'Enregistrer')));
    const d = h('dialog', {}, h('div', { class: 'body' }, h('h2', {}, title), form));
    document.body.append(d); d.showModal(); d.addEventListener('close', () => d.remove());
    return d;
  }

  // ---------- tâches d'infrastructure ----------
  // Une opération d'infrastructure n'est pas exécutée par control mais par le runner : on dépose
  // une tâche, puis on suit son journal. Le navigateur enchaîne les étapes ; s'il se ferme, la
  // tâche continue et l'état du nœud dit où l'on en est.
  function logView() {
    const pre = h('pre', { class: 'log' });
    return {
      el: pre,
      append: (t) => {
        const enBas = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 40;
        pre.append(document.createTextNode(t + '\n'));
        if (enBas) pre.scrollTop = pre.scrollHeight;
      },
    };
  }
  // Le flux SSE sert l'affichage, l'interrogation périodique sert la vérité : une coupure de flux
  // ne doit pas faire croire qu'une tâche a échoué.
  //
  // Et control disparaît pendant certaines tâches — un déploiement le redéploie. C'est précisément
  // ce que l'architecture permet : la tâche vit dans le runner. L'interface doit donc encaisser
  // une minute sans réponse au lieu de conclure à l'échec.
  async function waitJob(id, append) {
    const es = new EventSource('/v1/jobs/' + id + '/log');
    if (append) es.onmessage = (e) => append(e.data);
    let muet = 0;
    try {
      for (;;) {
        try {
          const j = await api('GET', '/v1/jobs/' + id, undefined, { noTenant: true });
          if (j.status !== 'pending' && j.status !== 'running') return j;
          if (muet) { append && append('(control a répondu de nouveau)'); muet = 0; }
        } catch (e) {
          if (++muet === 1) append && append('(control ne répond plus — probablement en cours de redéploiement ; la tâche, elle, continue)');
          if (muet > 60) throw e;
        }
        await new Promise((r) => setTimeout(r, 2000));
      }
    } finally { es.close(); }
  }
  async function runSteps(titre, etapes, apres) {
    const vue = logView();
    const etat = h('p', { class: 'steps' }, 'démarrage…');
    const fermer = h('button', { class: 'btn secondary', disabled: true, onclick: () => d.close() }, 'Fermer');
    const d = h('dialog', { class: 'large' }, h('div', { class: 'body' },
      h('h2', {}, titre), etat, vue.el,
      h('p', { class: 'hint' }, 'La tâche s\'exécute sur l\'hôte control, dans le runner : fermer cette fenêtre ne l\'interrompt pas. Si elle redéploie l\'hôte control, l\'interface se taira quelques secondes — le déploiement, lui, continue.'),
      h('div', { class: 'row' }, fermer)));
    document.body.append(d); d.showModal();
    d.addEventListener('close', () => { d.remove(); if (apres) apres(); });
    try {
      for (let i = 0; i < etapes.length; i++) {
        const e = etapes[i];
        etat.textContent = 'étape ' + (i + 1) + '/' + etapes.length + ' — ' + e.label;
        vue.append('\n=== ' + e.label + ' ===');
        if (e.run) { await e.run(); vue.append('fait'); continue; }
        const job = await api('POST', '/v1/jobs', { kind: e.kind || 'infra', request: e.request }, { noTenant: true });
        const fin = await waitJob(job.id, vue.append);
        if (fin.status !== 'succeeded') throw new Error(e.label + ' : ' + fin.status + ' (code ' + (fin.exit_code ?? '?') + ')');
      }
      etat.textContent = 'terminé';
      vue.append('\n=== terminé ===');
    } catch (err) {
      etat.textContent = 'échec : ' + err.message;
      vue.append('\n!! ' + err.message);
    } finally {
      fermer.disabled = false;
    }
  }

  // ---------- coquille ----------
  const routes = {};
  function nav() {
    const isPlatform = state.account && state.account.platform_admin;
    const links = [['#/dashboard', 'Tableau de bord'], ['#/rooms', 'Files d\'attente'], ['#/domains', 'Domaines'], ['#/backends', 'Backends'], ['#/integration', 'Intégration']];
    const platformLinks = [['#/platform/tenants', 'Tenants'], ['#/platform/users', 'Utilisateurs'], ['#/platform/infra', 'Infrastructure'], ['#/platform/jobs', 'Tâches']];
    const cur = location.hash.split('?')[0];
    const tenantSel = h('select', { class: 'tenant', onchange: (e) => { state.tenant = e.target.value; localStorage.setItem('flyc.tenant', state.tenant); render(); } },
      state.tenants.map(t => h('option', { value: t.slug, selected: t.slug === state.tenant }, t.name || t.slug)));
    return h('nav', { class: 'side' },
      h('div', { class: 'brand' }, 'Flyc'),
      state.tenants.length ? tenantSel : h('div', { class: 'hint', style: 'margin:0 10px 10px' }, 'Aucun tenant'),
      state.tenant ? links.map(([href, label]) => h('a', { href, class: cur === href ? 'active' : '' }, label)) : null,
      isPlatform ? [h('div', { class: 'section' }, 'Plateforme'), platformLinks.map(([href, label]) => h('a', { href, class: cur === href ? 'active' : '' }, label))] : null,
      h('div', { class: 'spacer' }),
      h('a', { href: '#/account', class: cur === '#/account' ? 'active' : '' }, state.account.email),
      h('a', { href: '#', onclick: async (e) => { e.preventDefault(); await api('POST', '/auth/logout'); state.account = null; location.hash = '#/login'; } }, 'Déconnexion'));
  }
  function page(title, sub, ...content) {
    return h('main', { class: 'content' }, h('h1', {}, title), sub ? h('p', { class: 'sub' }, sub) : null, ...content);
  }
  let pollTimer = null;
  async function render() {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
    const hash = location.hash || '#/dashboard';
    const [path, qs] = hash.split('?');
    const q = new URLSearchParams(qs || '');
    if (!state.account) {
      try { state.account = await api('GET', '/v1/account', undefined, { noTenant: true }); } catch (e) { state.account = null; }
    }
    if (!state.account) { $app.replaceChildren(await routes['#/login'](q)); return; }
    if (state.account.must_change_password && path !== '#/account') { location.hash = '#/account?force=1'; return; }
    await loadTenants();
    const view = routes[path] || routes['#/dashboard'];
    let body;
    try { body = await view(q); } catch (e) { body = page('Erreur', e.message); }
    $app.replaceChildren(h('div', { class: 'layout' }, nav(), body));
  }
  async function loadTenants() {
    if (state.account.platform_admin) {
      state.tenants = await api('GET', '/v1/tenants', undefined, { noTenant: true });
    } else {
      state.tenants = state.account.memberships.map(m => ({ slug: m.tenant, name: m.name, role: m.role }));
    }
    if (!state.tenants.find(t => t.slug === state.tenant)) state.tenant = state.tenants.length ? state.tenants[0].slug : '';
  }
  const role = () => state.account.platform_admin ? 'owner' : ((state.tenants.find(t => t.slug === state.tenant) || {}).role || 'viewer');
  const can = (min) => ({ viewer: 1, operator: 2, admin: 3, owner: 4 })[role()] >= ({ viewer: 1, operator: 2, admin: 3, owner: 4 })[min];

  // ---------- connexion ----------
  routes['#/login'] = async (q) => {
    try { state.authConfig = await api('GET', '/auth/config'); } catch (e) { state.authConfig = {}; }
    const email = h('input', { type: 'email', required: true, autocomplete: 'username' });
    const pw = h('input', { type: 'password', required: true, autocomplete: 'current-password' });
    const totp = h('input', { type: 'text', inputmode: 'numeric', placeholder: '000000', autocomplete: 'one-time-code' });
    const totpRow = h('label', { hidden: true }, 'Code de validation', totp);
    const err = h('p', { class: 'hint', style: 'color:var(--bad)' }, q.get('error') || '');
    const form = h('form', { class: 'stack', onsubmit: async (e) => {
      e.preventDefault(); err.textContent = '';
      try {
        const r = await api('POST', '/auth/login', { email: email.value, password: pw.value, totp: totp.value });
        state.account = null; location.hash = r.must_change_password ? '#/account?force=1' : '#/dashboard'; render();
      } catch (ex) {
        if (ex.status === 428) { totpRow.hidden = false; totp.focus(); err.textContent = 'Saisissez le code de votre application d\'authentification.'; }
        else err.textContent = ex.message;
      }
    } },
      h('label', {}, 'Email', email), h('label', {}, 'Mot de passe', pw), totpRow, err,
      h('button', { type: 'submit', class: 'btn' }, 'Se connecter'),
      state.authConfig.oidc ? h('a', { class: 'btn secondary', href: '/auth/oidc/start?redirect=/' }, 'Se connecter avec ' + (new URL(state.authConfig.oidc_issuer).hostname)) : null);
    return h('div', { class: 'login' }, h('div', { class: 'card' }, h('div', { class: 'brand', style: 'color:var(--accent);font-weight:700;letter-spacing:.04em;margin-bottom:6px' }, 'Flyc'), h('h1', {}, 'Administration'), form));
  };

  // ---------- tableau de bord ----------
  routes['#/dashboard'] = async () => {
    if (!state.tenant) return page('Bienvenue', 'Votre compte n\'est rattaché à aucun tenant. Un administrateur plateforme peut vous en attribuer un.');
    const rooms = await api('GET', '/v1/rooms');
    const cards = h('div', { class: 'grid cols-2' });
    async function refresh() {
      const items = await Promise.all(rooms.map(async r => { try { return [r, await api('GET', '/v1/rooms/' + r.slug + '/stats')]; } catch (e) { return [r, null]; } }));
      cards.replaceChildren(...items.map(([r, s]) => h('div', { class: 'card' },
        h('div', { class: 'row' }, h('strong', {}, r.title || r.slug), statePill(r.state), h('span', { class: 'hint right' }, r.domain || 'sans domaine')),
        s ? h('div', { class: 'grid cols-3', style: 'margin-top:12px' },
          h('div', { class: 'stat' }, h('span', { class: 'k' }, 'En attente'), h('b', {}, fmtN(s.pending + s.in_wave))),
          h('div', { class: 'stat' }, h('span', { class: 'k' }, 'Pass actifs'), h('b', {}, fmtN(s.active))),
          h('div', { class: 'stat' }, h('span', { class: 'k' }, 'Admis au total'), h('b', {}, fmtN((s.stats || {}).admitted_total)))) : h('p', { class: 'hint' }, 'statistiques indisponibles'),
        h('div', { class: 'row', style: 'margin-top:12px' }, stateButtons(r, async () => { const nr = await api('GET', '/v1/rooms'); rooms.splice(0, rooms.length, ...nr); refresh(); }),
          h('a', { class: 'right', href: '#/rooms' }, 'détails')))));
      if (!rooms.length) cards.replaceChildren(h('div', { class: 'card empty' }, 'Aucune file d\'attente. Créez-en une dans « Files d\'attente ».'));
    }
    await refresh();
    pollTimer = setInterval(refresh, 5000);
    let edgesCard = null;
    if (state.account.platform_admin) {
      const edges = await api('GET', '/v1/edges', undefined, { noTenant: true });
      edgesCard = h('div', { class: 'card' }, h('h2', { style: 'margin-top:0' }, 'Load balancers'),
        h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Nom'), h('th', {}, 'Synchronisation'), h('th', {}, 'Dernier push'), h('th', {}, 'Erreur'))),
          h('tbody', {}, edges.map(e => h('tr', {}, h('td', {}, e.name), h('td', {}, e.in_sync ? pill('ok', 'à jour') : pill('bad', e.last_push_status || 'inconnu')), h('td', {}, fmtDate(e.last_push_at)), h('td', { class: 'hint' }, e.last_error || ''))))));
    }
    return page('Tableau de bord', 'Files du tenant ' + state.tenant + ', actualisées toutes les 5 secondes.', cards, edgesCard);
  };
  function stateButtons(r, after) {
    const wrap = h('div', { class: 'state-btns' });
    for (const s of ['open', 'queue', 'closed', 'auto']) {
      const disabled = !can('operator') || (s === 'auto' && !(r.auto_on > 0));
      wrap.append(h('button', { class: (r.state === s ? 'on ' + s : ''), disabled, title: { open: 'Tout le monde passe', queue: 'File d\'attente active', closed: 'Fermé, page d\'attente fermée', auto: r.auto_on > 0 ? 'File activée au-dessus de ' + r.auto_on + ' connexions backend, désactivée sous ' + r.auto_off : 'Définissez les seuils dans la file' }[s],
        onclick: async () => { try { await api('POST', '/v1/rooms/' + r.slug + '/state', { state: s }); toast('File ' + r.slug + ' : ' + s + ' (LB à jour)'); r.state = s; after && after(); } catch (e) { toast(e.message, true); } } },
        { open: 'Ouvert', queue: 'File', closed: 'Fermé', auto: 'Auto' }[s]));
    }
    if (r.state === 'auto') wrap.append(h('span', { class: 'pill ' + ({ queue: 'wait', open: 'ok', closed: 'bad' }[r.effective_state] || 'muted'), style: 'margin-left:6px', title: 'état effectif décidé par la charge' }, r.effective_state));
    return wrap;
  }

  // ---------- files ----------
  routes['#/rooms'] = async () => {
    const [rooms, domains, backends] = await Promise.all([api('GET', '/v1/rooms'), api('GET', '/v1/domains'), api('GET', '/v1/backends')]);
    const edit = (r) => formDialog(r ? 'Modifier la file ' + r.slug : 'Nouvelle file', [
      { name: 'slug', label: 'Identifiant (minuscules, chiffres, tirets)', value: r ? r.slug : '', required: true, hint: r ? 'non modifiable' : '' },
      { name: 'title', label: 'Titre affiché dans la salle d\'attente', value: r ? r.title : '' },
      { name: 'domain', label: 'Domaine protégé', type: 'select', value: r ? r.domain : '', options: [['', '— aucun —'], ...domains.map(d => [d.fqdn, d.fqdn])] },
      { name: 'backend', label: 'Backend (serveurs du client)', type: 'select', value: r ? r.backend : '', options: [['', '— aucun —'], ...backends.map(b => [b.name, b.name])] },
      { name: 'state', label: 'État', type: 'select', value: r ? r.state : 'open', options: [['open', 'Ouvert'], ['queue', 'File active'], ['closed', 'Fermé'], ['auto', 'Automatique selon la charge']] },
      { name: 'auto_on', label: 'Auto : activer la file au-dessus de N connexions backend', type: 'number', min: '0', value: r ? r.auto_on : 0, hint: 'somme des sessions courantes vers les serveurs du backend, tous LB confondus' },
      { name: 'auto_off', label: 'Auto : la désactiver en dessous de N connexions', type: 'number', min: '0', value: r ? r.auto_off : 0 },
      { name: 'rate', label: 'Admissions par seconde', type: 'number', step: '0.1', min: '0.1', value: r ? r.rate : 10 },
      { name: 'max_active', label: 'Pass actifs maximum (0 = illimité)', type: 'number', min: '0', value: r ? r.max_active : 0 },
      { name: 'wave_s', label: 'Fenêtre de vague (secondes)', type: 'number', min: '1', value: r ? r.wave_s : 30, hint: 'les arrivées d\'une même fenêtre sont mélangées' },
      { name: 'pass_ttl_s', label: 'Validité du pass (secondes)', type: 'number', min: '60', value: r ? r.pass_ttl_s : 1200 },
      { name: 'opens_at', label: 'Ouverture prévue (ISO 8601, optionnel)', value: r && r.opens_at ? r.opens_at : '', placeholder: '2026-10-01T18:00:00+02:00' },
      { name: 'accent', label: 'Thème : couleur d\'accent (#hex)', value: r && r.theme ? r.theme.accent || '' : '', placeholder: '#0e7c86' },
      { name: 'logo_url', label: 'Thème : URL du logo (https)', value: r && r.theme ? r.theme.logo_url || '' : '' },
      { name: 'message', label: 'Thème : message affiché', value: r && r.theme ? r.theme.message || '' : '' },
    ], async (v) => {
      const body = { slug: v.slug, title: v.title, domain: v.domain, backend: v.backend, state: v.state, rate: Number(v.rate), max_active: Number(v.max_active), wave_s: Number(v.wave_s), pass_ttl_s: Number(v.pass_ttl_s), opens_at: v.opens_at || null, auto_on: Number(v.auto_on), auto_off: Number(v.auto_off), theme: { accent: v.accent, logo_url: v.logo_url, message: v.message } };
      if (r) await api('PUT', '/v1/rooms/' + r.slug, body); else await api('POST', '/v1/rooms', body);
      toast('File enregistrée, LB en cours de mise à jour'); render();
    });
    const table = h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'File'), h('th', {}, 'Domaine'), h('th', {}, 'Backend'), h('th', {}, 'État'), h('th', {}, 'Débit'), h('th', {}, 'Vague'), h('th', {}, ''))),
      h('tbody', {}, rooms.map(r => h('tr', {}, h('td', {}, h('strong', {}, r.title || r.slug), h('div', { class: 'hint mono' }, r.id)), h('td', {}, r.domain || '–'), h('td', {}, r.backend || '–'),
        h('td', {}, stateButtons(r)), h('td', {}, r.rate + '/s'), h('td', {}, r.wave_s + ' s'),
        h('td', { class: 'row' }, can('admin') ? h('button', { class: 'btn small secondary', onclick: () => edit(r) }, 'Modifier') : null,
          can('operator') ? h('button', { class: 'btn small secondary', onclick: () => confirmDialog('Vider la file', 'Tous les tickets en attente de ' + r.slug + ' seront supprimés. Les pass déjà émis restent valides.', () => api('POST', '/v1/rooms/' + r.slug + '/flush').then(() => toast('File vidée')), true) }, 'Vider') : null,
          can('admin') ? h('button', { class: 'btn small danger', onclick: () => confirmDialog('Supprimer la file', r.slug + ' sera retirée des LB.', () => api('DELETE', '/v1/rooms/' + r.slug).then(render), true) }, 'Supprimer') : null)))));
    return page('Files d\'attente', 'Une file protège un domaine et envoie les visiteurs admis vers un backend.',
      h('div', { class: 'row', style: 'margin-bottom:12px' }, can('admin') ? h('button', { class: 'btn', onclick: () => edit(null) }, '+ Nouvelle file') : null),
      h('div', { class: 'card' }, rooms.length ? table : h('div', { class: 'empty' }, 'Aucune file.')));
  };

  // ---------- domaines ----------
  routes['#/domains'] = async () => {
    const domains = await api('GET', '/v1/domains');
    const certPill = (c) => ({ issued: pill('ok', 'certificat ok'), pending: pill('wait', 'en cours'), error: pill('bad', 'erreur'), none: pill('muted', 'aucun') }[c.status] || pill('muted', c.status));
    const table = h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Domaine'), h('th', {}, 'Mode'), h('th', {}, 'Certificat'), h('th', {}, 'Expire'), h('th', {}, 'File'), h('th', {}, ''))),
      h('tbody', {}, domains.map(d => h('tr', {}, h('td', { class: 'mono' }, d.fqdn), h('td', {}, d.mode === 'dns' ? 'DNS (proxy complet)' : 'iframe / JS'),
        h('td', {}, certPill(d.certificate), d.certificate.last_error ? h('div', { class: 'hint', title: d.certificate.last_error }, d.certificate.last_error.slice(0, 80)) : null),
        h('td', {}, d.certificate.not_after ? fmtDate(d.certificate.not_after).slice(0, 10) : '–'), h('td', {}, d.room || '–'),
        h('td', { class: 'row' }, can('admin') ? h('button', { class: 'btn small secondary', onclick: () => api('POST', '/v1/domains/' + d.fqdn + '/certificate/renew').then(() => toast('Réémission demandée')).catch(e => toast(e.message, true)) }, 'Réémettre') : null,
          can('admin') ? h('button', { class: 'btn small danger', onclick: () => confirmDialog('Supprimer le domaine', d.fqdn + ' ne sera plus routé par les LB.', () => api('DELETE', '/v1/domains/' + d.fqdn).then(render), true) }, 'Supprimer') : null)))));
    const add = () => formDialog('Nouveau domaine', [
      { name: 'fqdn', label: 'Nom de domaine', placeholder: 'billets.exemple.fr', required: true, hint: 'Mode DNS : le client pointe ce nom sur la VIP (A/AAAA ou CNAME).' },
      { name: 'mode', label: 'Mode d\'intégration', type: 'select', value: 'dns', options: [['dns', 'DNS : tout le trafic passe par Flyc'], ['iframe', 'iframe / JS : le client garde son DNS']] },
      { name: 'tls', label: 'Certificat', type: 'select', value: 'auto', options: [['auto', 'Automatique (Let\'s Encrypt)'], ['none', 'Aucun']] },
    ], async (v) => { await api('POST', '/v1/domains', v); toast('Domaine créé, certificat en cours'); render(); }, 'Créer');
    return page('Domaines', 'Les domaines protégés et l\'état de leurs certificats.',
      h('div', { class: 'row', style: 'margin-bottom:12px' }, can('admin') ? h('button', { class: 'btn', onclick: add }, '+ Nouveau domaine') : null),
      h('div', { class: 'card' }, domains.length ? table : h('div', { class: 'empty' }, 'Aucun domaine.')));
  };

  // ---------- backends ----------
  routes['#/backends'] = async () => {
    const backends = await api('GET', '/v1/backends');
    function editBackend(b) {
      const servers = h('div', { class: 'servers' });
      const addSrv = (s) => {
        const row = h('div', { class: 'srv' },
          h('label', {}, 'Adresse', h('input', { name: 'address', required: true, value: s ? s.address : '', placeholder: '10.0.0.10' })),
          h('label', {}, 'Port', h('input', { name: 'port', type: 'number', min: '1', max: '65535', required: true, value: s ? s.port : 80 })),
          h('label', {}, 'Poids', h('input', { name: 'weight', type: 'number', min: '0', max: '256', value: s ? s.weight : 100 })),
          h('button', { type: 'button', class: 'btn small secondary', onclick: () => row.remove() }, '×'));
        servers.append(row);
      };
      (b ? b.servers : [{}]).forEach(addSrv);
      const name = h('input', { required: true, value: b ? b.name : '', placeholder: 'shop', readOnly: !!b });
      const check = h('input', { value: b ? b.check_path : '/', placeholder: '/health' });
      const balance = h('select', {}, [['roundrobin', 'Round-robin'], ['leastconn', 'Moins de connexions'], ['source', 'Par IP source']].map(o => h('option', { value: o[0], selected: b && b.balance === o[0] }, o[1])));
      const form = h('form', { class: 'stack', onsubmit: async (e) => {
        e.preventDefault();
        const srv = [...servers.querySelectorAll('.srv')].map((row, i) => ({ name: 'srv' + (i + 1), address: row.querySelector('[name=address]').value.trim(), port: Number(row.querySelector('[name=port]').value), weight: Number(row.querySelector('[name=weight]').value) }));
        try { const body = { name: name.value, balance: balance.value, check_path: check.value, servers: srv }; if (b) await api('PUT', '/v1/backends/' + b.name, body); else await api('POST', '/v1/backends', body); toast('Backend enregistré, LB en cours de mise à jour'); d.close(); render(); } catch (err) { toast(err.message, true); }
      } },
        h('label', {}, 'Nom', name), h('label', {}, 'Répartition', balance), h('label', {}, 'Chemin de health check', check),
        h('h2', {}, 'Serveurs'), h('p', { class: 'hint' }, 'Les serveurs du client. Flyc ne s\'y connecte que pour envoyer les visiteurs admis et vérifier leur santé.'), servers,
        h('button', { type: 'button', class: 'btn small secondary', onclick: () => addSrv(null) }, '+ Serveur'),
        h('div', { class: 'row' }, h('button', { type: 'button', class: 'btn secondary', onclick: () => d.close() }, 'Annuler'), h('button', { type: 'submit', class: 'btn' }, 'Enregistrer')));
      const d = h('dialog', {}, h('div', { class: 'body' }, h('h2', {}, b ? 'Modifier ' + b.name : 'Nouveau backend'), form));
      document.body.append(d); d.showModal(); d.addEventListener('close', () => d.remove());
    }
    const table = h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Nom'), h('th', {}, 'Nom HAProxy'), h('th', {}, 'Répartition'), h('th', {}, 'Serveurs'), h('th', {}, ''))),
      h('tbody', {}, backends.map(b => h('tr', {}, h('td', {}, h('strong', {}, b.name)), h('td', { class: 'mono' }, b.haproxy_name), h('td', {}, b.balance),
        h('td', {}, b.servers.map(s => h('div', { class: 'mono' }, s.address + ':' + s.port + (s.weight !== 100 ? ' (poids ' + s.weight + ')' : '')))),
        h('td', { class: 'row' }, can('admin') ? h('button', { class: 'btn small secondary', onclick: () => editBackend(b) }, 'Modifier') : null,
          can('admin') ? h('button', { class: 'btn small danger', onclick: () => confirmDialog('Supprimer le backend', b.name + ' sera retiré des LB. Les files qui l\'utilisent n\'auront plus de destination.', () => api('DELETE', '/v1/backends/' + b.name).then(render), true) }, 'Supprimer') : null)))));
    return page('Backends', 'Les serveurs qui rendent le site du client, une fois le visiteur admis.',
      h('div', { class: 'row', style: 'margin-bottom:12px' }, can('admin') ? h('button', { class: 'btn', onclick: () => editBackend(null) }, '+ Nouveau backend') : null),
      h('div', { class: 'card' }, backends.length ? table : h('div', { class: 'empty' }, 'Aucun backend.')));
  };

  // ---------- intégration ----------
  routes['#/integration'] = async () => {
    const [keys, rooms] = await Promise.all([api('GET', '/v1/api-keys'), api('GET', '/v1/rooms')]);
    const origin = location.origin;
    const keysTable = h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Nom'), h('th', {}, 'Créée'), h('th', {}, 'Dernier usage'), h('th', {}, ''))),
      h('tbody', {}, keys.map(k => h('tr', {}, h('td', {}, k.name), h('td', {}, fmtDate(k.created_at)), h('td', {}, fmtDate(k.last_used_at)),
        h('td', {}, can('admin') ? h('button', { class: 'btn small danger', onclick: () => confirmDialog('Révoquer la clé', k.name + ' cessera de fonctionner immédiatement.', () => api('DELETE', '/v1/api-keys/' + k.id).then(render), true) }, 'Révoquer') : null)))));
    const newKey = () => formDialog('Nouvelle clé API', [{ name: 'name', label: 'Nom', placeholder: 'intégration site', required: true }], async (v) => {
      const r = await api('POST', '/v1/api-keys', v);
      formDialog('Clé créée', [{ name: 'k', label: 'Copiez-la maintenant, elle ne sera plus affichée', value: r.api_key }], async () => { render(); }, 'Fermer');
    }, 'Créer');
    const waitHost = (state.authConfig && state.authConfig.wait_host) || 'wait.example';
    const snippet = (r) => `<script src="https://${waitHost}/widget.js" data-room="${state.tenant}.${r.slug}"><\/script>`;
    return page('Intégration', 'Clés API pour piloter Flyc depuis vos outils, et intégration côté site.',
      h('div', { class: 'card' }, h('div', { class: 'row' }, h('h2', { style: 'margin:0' }, 'Clés API du tenant'), can('admin') ? h('button', { class: 'btn small right', onclick: newKey }, '+ Nouvelle clé') : null),
        h('p', { class: 'hint' }, 'Usage : ', h('code', {}, 'Authorization: Bearer <clé>'), ' sur ', h('code', {}, origin + '/v1/…'), '. Documentation dans docs/api.md.'), keys.length ? keysTable : h('div', { class: 'empty' }, 'Aucune clé.')),
      h('div', { class: 'card' }, h('h2', { style: 'margin-top:0' }, 'Mode DNS'), h('p', {}, 'Pointez le domaine protégé sur la VIP de la plateforme (enregistrement A/AAAA ou CNAME). Le certificat est obtenu automatiquement. Aucune modification du site.'),
        h('p', {}, 'Pendant la file, HAProxy ajoute aux requêtes admises les en-têtes ', h('code', {}, 'X-Flyc-Room'), ' et ', h('code', {}, 'X-Flyc-Pass-Sub'), '.')),
      h('div', { class: 'card' }, h('h2', { style: 'margin-top:0' }, 'Mode iframe / JS'), h('p', {}, 'Le site garde son DNS. Il charge le widget, qui recouvre la page d\'une salle d\'attente tant que le visiteur n\'a pas de pass, puis pose le cookie ', h('code', {}, 'flyc_pass'), ' sur votre domaine et recharge la page. Déclarez le domaine du site en mode « iframe » et rattachez-le à la file.'),
        rooms.map(r => h('div', { class: 'code', style: 'margin-top:8px' }, snippet(r))),
        h('p', { class: 'hint', style: 'margin-top:10px' }, 'Ce mode protège l\'expérience, pas vos serveurs : la page initiale les atteint. Seul le mode DNS absorbe la charge.')),
      h('div', { class: 'card' }, h('h2', { style: 'margin-top:0' }, 'Vérification des pass côté serveur'),
        h('p', {}, 'Le pass est un JWT ES256 (claims ', h('code', {}, 'dom'), ', ', h('code', {}, 'rm'), ', ', h('code', {}, 'exp'), '). Vérifiez-le sur chaque requête protégée :'),
        h('ul', {}, h('li', {}, 'localement avec les SDK Node ou PHP du dépôt (', h('code', {}, 'sdk/'), '), clés publiques : ', h('code', {}, 'https://' + waitHost + '/.well-known/jwks.json')),
          h('li', {}, 'ou par l\'API : ', h('code', {}, 'POST ' + origin + '/v1/pass/verify'), ' avec ', h('code', {}, '{"pass": "…", "host": "votre-domaine"}'), ' et votre clé API'))));
  };

  // ---------- compte ----------
  routes['#/account'] = async (q) => {
    const acc = await api('GET', '/v1/account', undefined, { noTenant: true });
    state.account = acc;
    const force = q.get('force') === '1' || acc.must_change_password;
    const cur = h('input', { type: 'password', autocomplete: 'current-password' });
    const nw = h('input', { type: 'password', required: true, autocomplete: 'new-password', minlength: 12 });
    const pwForm = h('form', { class: 'stack', onsubmit: async (e) => { e.preventDefault(); try { await api('POST', '/v1/account/password', { current: cur.value, new: nw.value }); toast('Mot de passe changé'); state.account = null; location.hash = '#/dashboard'; render(); } catch (err) { toast(err.message, true); } } },
      force ? h('p', { style: 'color:var(--wait)' }, 'Vous devez définir un nouveau mot de passe avant de continuer.') : null,
      h('label', {}, 'Mot de passe actuel', cur), h('label', {}, 'Nouveau mot de passe (12 caractères minimum)', nw), h('button', { type: 'submit', class: 'btn' }, 'Changer'));
    let totpBox;
    if (acc.totp_enabled) {
      const pw = h('input', { type: 'password' });
      totpBox = h('form', { class: 'stack', onsubmit: async (e) => { e.preventDefault(); try { await api('POST', '/v1/account/totp/disable', { password: pw.value }); toast('Validation en deux étapes désactivée'); render(); } catch (err) { toast(err.message, true); } } },
        pill('ok', 'activée'), h('label', {}, 'Mot de passe pour désactiver', pw), h('button', { type: 'submit', class: 'btn danger' }, 'Désactiver'));
    } else {
      const box = h('div', { class: 'stack' });
      const start = h('button', { class: 'btn secondary', onclick: async () => {
        const r = await api('POST', '/v1/account/totp/setup');
        const code = h('input', { inputmode: 'numeric', placeholder: '000000', required: true });
        box.replaceChildren(h('p', {}, 'Scannez ce QR code avec votre application (Aegis, Google Authenticator, 1Password…), puis saisissez le code affiché.'),
          h('img', { src: '/v1/account/totp/qr.png?' + Date.now(), width: 220, height: 220, alt: 'QR code' }), h('div', { class: 'code' }, r.secret),
          h('form', { class: 'stack', onsubmit: async (e) => { e.preventDefault(); try { await api('POST', '/v1/account/totp/enable', { code: code.value }); toast('Validation en deux étapes activée'); render(); } catch (err) { toast(err.message, true); } } }, h('label', {}, 'Code', code), h('button', { type: 'submit', class: 'btn' }, 'Activer')));
      } }, 'Configurer');
      box.append(pill('muted', 'désactivée'), start);
      totpBox = box;
    }
    return page('Mon compte', acc.email + (acc.platform_admin ? ' · administrateur plateforme' : ''),
      h('div', { class: 'grid cols-2' }, h('div', { class: 'card' }, h('h2', { style: 'margin-top:0' }, 'Mot de passe'), pwForm),
        h('div', { class: 'card' }, h('h2', { style: 'margin-top:0' }, 'Validation en deux étapes (TOTP)'), totpBox)),
      acc.memberships.length ? h('div', { class: 'card' }, h('h2', { style: 'margin-top:0' }, 'Mes tenants'), h('table', {}, h('tbody', {}, acc.memberships.map(m => h('tr', {}, h('td', {}, m.name || m.tenant), h('td', {}, pill('info', m.role))))))) : null);
  };

  // ---------- plateforme ----------
  routes['#/platform/tenants'] = async () => {
    if (!state.account.platform_admin) return page('Accès refusé');
    const tenants = await api('GET', '/v1/tenants', undefined, { noTenant: true });
    const create = () => formDialog('Nouveau tenant', [{ name: 'slug', label: 'Identifiant', placeholder: 'acme', required: true }, { name: 'name', label: 'Nom', placeholder: 'ACME Billetterie' }], async (v) => {
      const r = await api('POST', '/v1/tenants', v, { noTenant: true });
      formDialog('Tenant créé', [{ name: 'k', label: 'Clé API initiale du tenant (affichée une seule fois)', value: r.api_key }], async () => { render(); }, 'Fermer');
    }, 'Créer');
    const members = async (t) => {
      const list = await api('GET', '/v1/tenants/' + t.slug + '/members', undefined, { noTenant: true });
      const d = h('dialog', {}, h('div', { class: 'body' }, h('h2', {}, 'Membres de ' + t.slug),
        h('table', {}, h('tbody', {}, list.map(m => h('tr', {}, h('td', {}, m.email), h('td', {}, pill('info', m.role)), h('td', {}, h('button', { class: 'btn small danger', onclick: async () => { await api('DELETE', '/v1/tenants/' + t.slug + '/members/' + encodeURIComponent(m.email), undefined, { noTenant: true }); d.close(); members(t); } }, 'Retirer')))))),
        h('button', { class: 'btn secondary', style: 'margin-top:12px', onclick: () => { d.close(); formDialog('Ajouter un membre', [{ name: 'email', label: 'Email d\'un utilisateur existant', type: 'email', required: true }, { name: 'role', label: 'Rôle', type: 'select', value: 'operator', options: [['viewer', 'Lecture seule'], ['operator', 'Opérateur : états et vidage'], ['admin', 'Administrateur du tenant'], ['owner', 'Propriétaire']] }], async (v) => { await api('PUT', '/v1/tenants/' + t.slug + '/members', v, { noTenant: true }); members(t); }, 'Ajouter'); } }, '+ Ajouter'),
        h('button', { class: 'btn', style: 'margin:12px 0 0 8px', onclick: () => d.close() }, 'Fermer')));
      document.body.append(d); d.showModal(); d.addEventListener('close', () => d.remove());
    };
    return page('Tenants', 'Chaque client de la plateforme.',
      h('div', { class: 'row', style: 'margin-bottom:12px' }, h('button', { class: 'btn', onclick: create }, '+ Nouveau tenant')),
      h('div', { class: 'card' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Identifiant'), h('th', {}, 'Nom'), h('th', {}, 'Files'), h('th', {}, 'Domaines'), h('th', {}, 'Créé'), h('th', {}, ''))),
        h('tbody', {}, tenants.map(t => h('tr', {}, h('td', { class: 'mono' }, t.slug), h('td', {}, t.name), h('td', {}, t.rooms), h('td', {}, t.domains), h('td', {}, fmtDate(t.created_at)),
          h('td', { class: 'row' }, h('button', { class: 'btn small secondary', onclick: () => members(t) }, 'Membres'), h('button', { class: 'btn small secondary', onclick: () => { state.tenant = t.slug; localStorage.setItem('flyc.tenant', t.slug); location.hash = '#/dashboard'; } }, 'Ouvrir'))))))));
  };
  routes['#/platform/users'] = async () => {
    if (!state.account.platform_admin) return page('Accès refusé');
    const [users, tenants] = await Promise.all([api('GET', '/v1/users', undefined, { noTenant: true }), api('GET', '/v1/tenants', undefined, { noTenant: true })]);
    const create = () => formDialog('Nouvel utilisateur', [
      { name: 'email', label: 'Email', type: 'email', required: true }, { name: 'display_name', label: 'Nom affiché' },
      { name: 'password', label: 'Mot de passe initial (vide : généré)', type: 'text' },
      { name: 'tenant', label: 'Rattacher au tenant', type: 'select', value: '', options: [['', '— aucun —'], ...tenants.map(t => [t.slug, t.slug])] },
      { name: 'role', label: 'Rôle sur ce tenant', type: 'select', value: 'operator', options: [['viewer', 'Lecture seule'], ['operator', 'Opérateur'], ['admin', 'Administrateur'], ['owner', 'Propriétaire']] },
      { name: 'platform_admin', label: 'Administrateur plateforme (accès à tout)', type: 'checkbox' },
    ], async (v) => {
      const r = await api('POST', '/v1/users', v, { noTenant: true });
      formDialog('Utilisateur créé', [{ name: 'p', label: 'Mot de passe initial (à transmettre, changement obligatoire à la première connexion)', value: r.initial_password }], async () => { render(); }, 'Fermer');
    }, 'Créer');
    return page('Utilisateurs', 'Comptes locaux et comptes venus du fournisseur OIDC.',
      h('div', { class: 'row', style: 'margin-bottom:12px' }, h('button', { class: 'btn', onclick: create }, '+ Nouvel utilisateur')),
      h('div', { class: 'card' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Email'), h('th', {}, 'Nom'), h('th', {}, 'Rôles'), h('th', {}, 'Sécurité'), h('th', {}, 'Dernière connexion'), h('th', {}, ''))),
        h('tbody', {}, users.map(u => h('tr', {}, h('td', {}, u.email), h('td', {}, u.display_name), h('td', {}, u.platform_admin ? pill('info', 'plateforme') : null, ' ', u.memberships ? u.memberships.split(',').map(m => h('span', { class: 'pill muted', style: 'margin-right:4px' }, m)) : null),
          h('td', {}, u.totp_enabled ? pill('ok', 'TOTP') : null, ' ', u.oidc ? pill('info', 'OIDC') : null), h('td', {}, fmtDate(u.last_login_at)),
          h('td', { class: 'row' }, h('button', { class: 'btn small secondary', onclick: () => confirmDialog('Réinitialiser le mot de passe', 'Un nouveau mot de passe sera généré pour ' + u.email + ' et ses sessions fermées.', async () => { const r = await api('POST', '/v1/users/' + u.id + '/password', {}, { noTenant: true }); formDialog('Nouveau mot de passe', [{ name: 'p', label: 'À transmettre', value: r.initial_password }], async () => {}, 'Fermer'); }) }, 'Mot de passe'),
            u.id !== undefined ? h('button', { class: 'btn small danger', onclick: () => confirmDialog('Supprimer l\'utilisateur', u.email, () => api('DELETE', '/v1/users/' + u.id, undefined, { noTenant: true }).then(render), true) }, 'Supprimer') : null)))))));
  };
  const roleNoms = { edge: 'LB', queue: 'file', data: 'control' };
  const nodePill = (e) => pill({ ready: 'ok', planned: 'info', installing: 'wait', draining: 'bad' }[e] || 'muted',
    { ready: 'en service', planned: 'déclaré', installing: 'installation', draining: 'en retrait' }[e] || e);

  // Les LB déjà en service, à qui déclarer un nouveau nœud par la Dataplane API.
  const ciblesLB = (nodes, sauf) => nodes.filter(n => n.state === 'ready' && n.roles.includes('edge') && n.name !== sauf)
    .map(n => n.name).join(',');
  const typeNode = (n) => n.roles.includes('edge') ? 'lb' : 'queue';

  function dialogueEnrolement(apres) {
    const roles = { edge: h('input', { type: 'checkbox' }), queue: h('input', { type: 'checkbox' }) };
    const zone = h('div', { class: 'stack' });
    const d = h('dialog', {}, h('div', { class: 'body' }, h('h2', {}, 'Ajouter un nœud'), zone));
    document.body.append(d); d.showModal(); d.addEventListener('close', () => { d.remove(); if (apres) apres(); });

    const etape1 = () => zone.replaceChildren(
      h('p', { class: 'hint' }, 'Un jeton à usage unique, valable quinze minutes, est créé. La machine à enrôler exécute une ligne qui y crée le compte de déploiement et y installe la clé publique de cet hôte : rien de secret ne circule.'),
      h('label', { class: 'row' }, roles.edge, ' Load balancer (edge)'),
      h('label', { class: 'row' }, roles.queue, ' Hôte de file (queue)'),
      h('div', { class: 'row' },
        h('button', { class: 'btn secondary', onclick: () => d.close() }, 'Annuler'),
        h('button', {
          class: 'btn', onclick: async (ev) => {
            const choisis = Object.entries(roles).filter(([, el]) => el.checked).map(([k]) => k);
            if (!choisis.length) return toast('choisir au moins un rôle', true);
            ev.target.disabled = true;
            try { etape2(await api('POST', '/v1/enroll/tokens', { roles: choisis }, { noTenant: true }), choisis); }
            catch (e) { ev.target.disabled = false; toast(e.message, true); }
          }
        }, 'Créer le jeton')));

    function etape2(jeton, choisis) {
      const cmd = h('pre', { class: 'log' }, jeton.command);
      const att = h('p', { class: 'steps' }, 'en attente de la machine…');
      zone.replaceChildren(
        h('p', {}, 'À coller sur la machine à enrôler :'), cmd,
        h('div', { class: 'row' },
          h('button', { class: 'btn secondary', onclick: () => { navigator.clipboard.writeText(jeton.command).then(() => toast('copié')); } }, 'Copier'),
          h('span', { class: 'hint' }, 'expire le ' + fmtDate(jeton.expires_at))),
        att,
        h('div', { class: 'row' }, h('button', { class: 'btn secondary', onclick: () => d.close() }, 'Fermer')));
      const timer = setInterval(async () => {
        let liste;
        try { liste = await api('GET', '/v1/enroll/tokens', undefined, { noTenant: true }); } catch (e) { return; }
        const t = liste.find(x => x.id === jeton.id);
        if (!t) { clearInterval(timer); att.textContent = 'jeton expiré'; return; }
        if (!t.used_at) return;
        clearInterval(timer);
        etape3(t, choisis);
      }, 3000);
      d.addEventListener('close', () => clearInterval(timer));
    }

    // La machine s'est annoncée : elle n'a pas encore de rôle, c'est ici qu'on le lui donne.
    function etape3(t, choisis) {
      const champs = {
        name: h('input', { value: t.hostname || '' }),
        mgmt_ip: h('input', { value: t.mgmt_ip || '' }),
        mgmt_ip6: h('input', { value: t.mgmt_ip6 || '' }),
      };
      for (const [k, el] of Object.entries(champs)) el.value = (t[k === 'name' ? 'hostname' : k]) || '';
      zone.replaceChildren(
        h('p', {}, 'Machine enrôlée : ', h('strong', {}, t.hostname || '?'), ' — vérifier puis déclarer.'),
        h('label', {}, 'Nom dans l\'inventaire', champs.name),
        h('label', {}, 'Adresse d\'administration', champs.mgmt_ip),
        h('label', {}, 'Adresse IPv6 (facultative)', champs.mgmt_ip6),
        h('p', { class: 'hint' }, 'Rôles : ' + choisis.map(r => roleNoms[r]).join(' + ') + '. Le nœud est déclaré, pas encore installé : son installation se lance depuis la liste.'),
        h('div', { class: 'row' },
          h('button', { class: 'btn secondary', onclick: () => d.close() }, 'Annuler'),
          h('button', {
            class: 'btn', onclick: async (ev) => {
              ev.target.disabled = true;
              try {
                await api('POST', '/v1/nodes', {
                  name: champs.name.value.trim(), roles: choisis,
                  mgmt_ip: champs.mgmt_ip.value.trim(), mgmt_ip6: champs.mgmt_ip6.value.trim(), state: 'planned',
                }, { noTenant: true });
                toast('nœud déclaré');
                d.close();
              } catch (e) { ev.target.disabled = false; toast(e.message, true); }
            }
          }, 'Déclarer le nœud')));
    }
    etape1();
  }

  function installerNode(n, nodes, apres) {
    const cibles = ciblesLB(nodes, n.name);
    const etapes = [{
      label: 'Installation de ' + n.name + ' et convergence de la plateforme',
      request: { kind: 'playbook', playbook: 'edge/site.yml' },
    }];
    if (cibles) {
      etapes.push({
        label: 'Déclaration sur les LB en service (' + cibles + ')',
        request: {
          kind: 'playbook', playbook: 'tools/propagate-node.yml',
          extra_vars: {
            propagate_kind: typeNode(n), propagate_name: n.name, propagate_ip: n.mgmt_ip,
            propagate_state: 'present', propagate_targets: cibles,
          },
        },
      });
    }
    etapes.push({
      label: 'Mise en service',
      run: async () => {
        await api('PATCH', '/v1/nodes/' + n.name, { state: 'ready' }, { noTenant: true });
        await api('POST', '/v1/sync', undefined, { noTenant: true });
      },
    });
    etapes.push({
      label: 'Vérification',
      request: { kind: 'script', argv: ['tools/check-node.sh', typeNode(n), n.name] },
    });
    runSteps('Installation de ' + n.name, etapes, apres);
  }

  function retirerNode(n, nodes, apres) {
    const type = typeNode(n);
    const restantes = nodes.filter(x => x.state === 'ready' && x.roles.includes('queue') && x.name !== n.name).length;
    const avert = type === 'queue'
      ? 'Ses deux nœuds Redis seront sortis du cluster : le vidage de leurs slots déplace de vraies clés, comptez quelques minutes, le service continue pendant l\'opération. Il resterait ' + restantes + ' hôte(s) de file (minimum 3).'
      : 'Vérifier d\'abord que BIRD est arrêté sur ' + n.name + ' et que le trafic a basculé : un LB qui annonce encore les VIP ne doit pas être éteint.';
    confirmDialog('Retirer ' + n.name,
      avert + ' Les services seront arrêtés et désactivés, les VIP retirées ; tout reste installé.',
      async () => {
        const cibles = ciblesLB(nodes, n.name);
        const etapes = [{
          label: 'Mise en retrait',
          run: () => api('PATCH', '/v1/nodes/' + n.name, { state: 'draining' }, { noTenant: true }),
        }];
        if (type === 'queue') {
          etapes.push({
            label: 'Sortie des nœuds Redis du cluster',
            request: { kind: 'playbook', playbook: 'tools/redis-detach.yml', extra_vars: { detach_node: n.name } },
          });
        }
        if (cibles) {
          etapes.push({
            label: 'Retrait de la configuration des LB en service',
            request: {
              kind: 'playbook', playbook: 'tools/propagate-node.yml',
              extra_vars: {
                propagate_kind: type, propagate_name: n.name, propagate_ip: n.mgmt_ip,
                propagate_state: 'absent', propagate_targets: cibles,
              },
            },
          });
        }
        etapes.push({
          label: 'Mise hors service de ' + n.name,
          request: {
            kind: 'playbook', playbook: 'tools/decommission-node.yml', limit: n.name,
            extra_vars: { decommission_level: 'inert', decommission_role: type },
          },
        });
        etapes.push({
          label: 'Retrait de l\'inventaire',
          run: () => api('DELETE', '/v1/nodes/' + n.name, undefined, { noTenant: true }),
        });
        // Les règles de pare-feu des hôtes restants énumèrent les adresses autorisées : sans cette
        // passe, ils continueraient d'accepter l'ancien nœud.
        etapes.push({
          label: 'Convergence des nœuds restants',
          request: { kind: 'playbook', playbook: 'edge/site.yml', tags: type === 'queue' ? 'firewall,compose' : 'firewall' },
        });
        runSteps('Retrait de ' + n.name, etapes, apres);
      }, true);
  }

  function editerReglages(s, apres) {
    const voisins = (s.bgp_neighbors || []).map(v => v.ip + ' ' + v.remote_as + ' ' + v.family).join('\n');
    formDialog('Réglages de la plateforme', [
      { name: 'wait_host', label: 'Domaine de la salle d\'attente', value: s.wait_host },
      { name: 'control_host', label: 'Domaine de l\'interface', value: s.control_host },
      { name: 'control_public_url', label: 'URL publique de l\'interface', value: s.control_public_url },
      { name: 'image_tag', label: 'Version des images', value: s.image_tag, hint: 'C\'est cette valeur qui est déployée, pas celle d\'un fichier d\'inventaire.' },
      { name: 'registry', label: 'Registre d\'images', value: s.registry },
      { name: 'vip_app_v4', label: 'VIP application (v4)', value: s.vip_app_v4 },
      { name: 'vip_app_v6', label: 'VIP application (v6)', value: s.vip_app_v6 },
      { name: 'vip_wait_v4', label: 'VIP salle d\'attente (v4)', value: s.vip_wait_v4 },
      { name: 'vip_wait_v6', label: 'VIP salle d\'attente (v6)', value: s.vip_wait_v6 },
      { name: 'vip_gateway_v4', label: 'Passerelle de retour des VIP (v4)', value: s.vip_gateway_v4 },
      { name: 'vip_gateway_v6', label: 'Passerelle de retour des VIP (v6)', value: s.vip_gateway_v6 },
      { name: 'bgp_enabled', label: 'Annonce BGP des VIP', type: 'checkbox', value: s.bgp_enabled },
      { name: 'bgp_local_as', label: 'AS local', type: 'number', value: s.bgp_local_as },
      { name: 'bgp_neighbors', label: 'Voisins BGP', type: 'textarea', value: voisins, hint: 'Une ligne par voisin : adresse, AS distant, v4 ou v6.' },
      { name: 'acme_email', label: 'Contact ACME', value: s.acme_email },
      { name: 'acme_directory', label: 'Directory ACME', value: s.acme_directory, hint: 'Vide = Let\'s Encrypt production.' },
      { name: 'deploy_from', label: 'Adresse autorisée pour la clé de déploiement', value: s.deploy_from },
      { name: 'manage_firewall', label: 'Gérer le pare-feu (ufw)', type: 'checkbox', value: s.manage_firewall },
    ], async (v) => {
      const corps = Object.assign({}, v);
      corps.bgp_local_as = Number(v.bgp_local_as || 0);
      corps.bgp_neighbors = (v.bgp_neighbors || '').split('\n').map(l => l.trim()).filter(Boolean).map(l => {
        const [ip, as, fam] = l.split(/[\s,]+/);
        return { ip, remote_as: Number(as || 0), family: fam || (ip && ip.includes(':') ? 'v6' : 'v4') };
      });
      await api('PUT', '/v1/settings', corps, { noTenant: true });
      toast('réglages enregistrés');
      if (apres) apres();
    });
  }

  routes['#/platform/infra'] = async () => {
    if (!state.account.platform_admin) return page('Accès refusé');
    const [nodes, edges, hosts, keys, reglages] = await Promise.all([
      api('GET', '/v1/nodes', undefined, { noTenant: true }),
      api('GET', '/v1/edges', undefined, { noTenant: true }),
      api('GET', '/v1/queue-hosts', undefined, { noTenant: true }),
      api('GET', '/v1/keys', undefined, { noTenant: true }),
      api('GET', '/v1/settings', undefined, { noTenant: true }),
    ]);
    const etatEdge = Object.fromEntries(edges.map(e => [e.name, e]));
    const etatQueue = Object.fromEntries(hosts.map(q => [q.name, q]));
    const sante = (n) => {
      if (n.state !== 'ready') return h('span', { class: 'hint' }, '–');
      if (n.roles.includes('edge')) {
        const e = etatEdge[n.name];
        return e ? (e.in_sync ? pill('ok', 'à jour') : pill('bad', e.last_push_status || 'inconnu')) : pill('muted', 'inconnu');
      }
      if (n.roles.includes('queue')) {
        const q = etatQueue[n.name];
        return q ? (q.health === 'ok' ? pill('ok', 'ok') : pill('bad', q.health)) : pill('muted', 'inconnu');
      }
      return pill('ok', 'ok');
    };
    const recharger = () => render();
    const ligne = (n) => h('tr', {},
      h('td', {}, h('strong', {}, n.name), n.note ? h('div', { class: 'hint' }, n.note) : null),
      h('td', {}, n.roles.map(r => roleNoms[r] || r).join(' + ')),
      h('td', { class: 'mono' }, n.mgmt_ip, n.mgmt_ip6 ? h('div', { class: 'hint mono' }, n.mgmt_ip6) : null),
      h('td', {}, nodePill(n.state)),
      h('td', {}, sante(n)),
      h('td', { class: 'right' }, h('div', { class: 'row' },
        n.state === 'planned' ? h('button', { class: 'btn small', onclick: () => installerNode(n, nodes, recharger) }, 'Installer') : null,
        n.state === 'ready' && !n.roles.includes('data') ? h('button', { class: 'btn secondary small', onclick: () => retirerNode(n, nodes, recharger) }, 'Retirer') : null,
        n.state !== 'ready' && !n.roles.includes('data') ? h('button', {
          class: 'btn secondary small', onclick: () => confirmDialog('Oublier ' + n.name,
            'Le nœud sort de l\'inventaire. La machine n\'est pas touchée : ses services continuent de tourner si elle a été installée.',
            async () => { await api('DELETE', '/v1/nodes/' + n.name, undefined, { noTenant: true }); toast('nœud retiré de l\'inventaire'); render(); }, true)
        }, 'Oublier') : null)));

    return page('Infrastructure', 'Les nœuds déclarés ici sont la vérité : l\'inventaire Ansible en est la projection.',
      h('div', { class: 'row', style: 'margin-bottom:12px' },
        h('button', { class: 'btn', onclick: () => dialogueEnrolement(recharger) }, 'Ajouter un nœud'),
        h('button', { class: 'btn secondary', onclick: () => editerReglages(reglages, recharger) }, 'Réglages de la plateforme'),
        h('button', { class: 'btn secondary', onclick: () => api('POST', '/v1/sync', undefined, { noTenant: true }).then(() => { toast('Synchronisation lancée'); render(); }) }, 'Synchroniser maintenant'),
        h('button', {
          class: 'btn secondary', onclick: () => runSteps('Déploiement de toute la plateforme',
            [{ label: 'edge/site.yml sur tous les nœuds', request: { kind: 'playbook', playbook: 'edge/site.yml' } }], recharger)
        }, 'Déployer la plateforme')),
      h('div', { class: 'card' },
        h('h2', { style: 'margin-top:0' }, 'Nœuds'),
        h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Nom'), h('th', {}, 'Rôles'), h('th', {}, 'Adresses'), h('th', {}, 'État'), h('th', {}, 'Santé'), h('th', {}))),
          h('tbody', {}, nodes.length ? nodes.map(ligne) : h('tr', {}, h('td', { colspan: 6, class: 'hint' }, 'aucun nœud déclaré')))),
        h('p', { class: 'hint' }, 'Version déployée : ' + reglages.image_tag + '. Un nœud déclaré n\'est pas installé tant que sa tâche d\'installation n\'a pas réussi.')),
      h('div', { class: 'card' }, h('div', { class: 'row' }, h('h2', { style: 'margin:0' }, 'Clés de signature des pass'),
          h('button', { class: 'btn secondary small right', onclick: () => confirmDialog('Faire tourner la clé', 'Une nouvelle clé ES256 devient active. L\'ancienne reste acceptée 24 h par les LB, le JWKS et la vérification, le temps que les pass en cours expirent. Les LB sont rechargés.', async () => { const r = await api('POST', '/v1/keys/rotate', {}, { noTenant: true }); toast('Nouvelle clé ' + r.kid); render(); }) }, 'Faire tourner')),
        h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'kid'), h('th', {}, 'État'), h('th', {}, 'Créée'), h('th', {}, 'Retirée'))),
          h('tbody', {}, keys.map(k => h('tr', {}, h('td', { class: 'mono' }, k.kid), h('td', {}, k.active ? pill('ok', 'active') : (k.accepted ? pill('wait', 'acceptée 24 h') : pill('muted', 'retirée'))), h('td', {}, fmtDate(k.created_at)), h('td', {}, fmtDate(k.retired_at))))))));
  };

  routes['#/platform/jobs'] = async () => {
    if (!state.account.platform_admin) return page('Accès refusé');
    const jobs = await api('GET', '/v1/jobs?limit=30', undefined, { noTenant: true });
    const jobPill = (j) => pill({ succeeded: 'ok', running: 'wait', pending: 'info', failed: 'bad', interrupted: 'bad' }[j.status] || 'muted', j.status);
    const duree = (j) => {
      if (!j.started_at || !j.finished_at) return '–';
      const s = Math.round((new Date(j.finished_at) - new Date(j.started_at)) / 1000);
      return s < 60 ? s + ' s' : Math.floor(s / 60) + ' min ' + (s % 60) + ' s';
    };
    const quoi = (j) => (j.request && (j.request.playbook || (j.request.argv || []).join(' '))) || j.kind;
    const voir = async (j) => {
      const vue = logView();
      const d = h('dialog', { class: 'large' }, h('div', { class: 'body' }, h('h2', {}, quoi(j)),
        h('p', { class: 'steps' }, j.status + (j.exit_code !== undefined && j.exit_code !== null ? ' (code ' + j.exit_code + ')' : '')),
        vue.el, h('div', { class: 'row' }, h('button', { class: 'btn secondary', onclick: () => d.close() }, 'Fermer'))));
      document.body.append(d); d.showModal();
      const es = new EventSource('/v1/jobs/' + j.id + '/log');
      es.onmessage = (e) => vue.append(e.data);
      es.addEventListener('end', () => es.close());
      d.addEventListener('close', () => { es.close(); d.remove(); render(); });
    };
    // Une tâche en cours : on rafraîchit la liste, le journal se suit en l'ouvrant.
    if (jobs.some(j => j.status === 'pending' || j.status === 'running')) pollTimer = setInterval(render, 5000);
    return page('Tâches', 'Les opérations d\'infrastructure sont exécutées par le runner sur l\'hôte control, pas par cette interface.',
      h('div', { class: 'card' },
        h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Opération'), h('th', {}, 'État'), h('th', {}, 'Lancée'), h('th', {}, 'Durée'), h('th', {}, 'Par'), h('th', {}))),
          h('tbody', {}, jobs.length ? jobs.map(j => h('tr', {},
            h('td', { class: 'mono' }, quoi(j), j.request && j.request.limit ? h('div', { class: 'hint' }, 'limité à ' + j.request.limit) : null),
            h('td', {}, jobPill(j), j.detail ? h('div', { class: 'hint' }, j.detail) : null),
            h('td', {}, fmtDate(j.created_at)),
            h('td', {}, duree(j)),
            h('td', { class: 'hint' }, j.created_by || '–'),
            h('td', { class: 'right' }, h('button', { class: 'btn secondary small', onclick: () => voir(j) }, 'Journal'))))
            : h('tr', {}, h('td', { colspan: 6, class: 'hint' }, 'aucune tâche'))))));
  };

  window.addEventListener('hashchange', render);
  render();
})();
