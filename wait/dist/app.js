/* Salle d'attente Flyc.
   Flux : POST /q/<room>/ticket → SSE /q/<room>/events (repli : polling /status) → admitted → /q/<room>/go → site. */
(function () {
  'use strict';
  const q = new URLSearchParams(location.search);
  const room = decodeURIComponent(location.pathname.replace(/^\/w\//, '').split('/')[0] || '');
  const host = (q.get('h') || '').toLowerCase();
  const back = q.get('r') || '';
  const closedHint = q.get('closed') === '1';
  // Mode iframe / JS : la page tourne dans un cadre sur le site du client ; le pass lui est transmis par postMessage.
  const iframeMode = q.get('mode') === 'iframe' && window.parent !== window;
  const parentOrigin = (() => { try { const o = new URL(q.get('o') || ''); return (o.protocol === 'https:' || o.protocol === 'http:') && o.hostname.toLowerCase() === host ? o.origin : ''; } catch (e) { return ''; } })();
  const $ = (id) => document.getElementById(id);
  const root = $('room');
  const base = '/q/' + encodeURIComponent(room);
  let ticket = null, es = null, pollTimer = null, pollEvery = 5, lastStatusAt = 0, redirecting = false;

  const setState = (s) => { root.dataset.state = s; };
  const text = (id, v) => { $(id).textContent = v; };
  const fmtEta = (s) => {
    if (s <= 0) return 'quelques secondes';
    if (s < 60) return 'moins d\'une minute';
    const m = Math.round(s / 60);
    if (m < 60) return 'environ ' + m + ' min';
    const h = Math.floor(m / 60);
    return 'environ ' + h + ' h ' + String(m % 60).padStart(2, '0');
  };
  const fmtN = (n) => n.toLocaleString('fr-FR');

  function fail(title, lede, retry) {
    setState('error');
    text('eyebrow', 'Salle d\'attente');
    text('title', title);
    text('lede', lede);
    $('stats').hidden = true;
    $('retry').hidden = !retry;
  }

  function showClosed(opensAt) {
    stopAll();
    if (iframeMode && parentOrigin) window.parent.postMessage({ type: 'flyc:closed', opens_at: opensAt || 0 }, parentOrigin);
    setState('closed');
    text('eyebrow', 'Fermé pour le moment');
    text('title', 'La billetterie n\'est pas encore ouverte');
    text('lede', opensAt ? 'Ouverture prévue le ' + new Date(opensAt * 1000).toLocaleString('fr-FR') + '. Cette page se mettra à jour toute seule.' : 'Revenez un peu plus tard, cette page se mettra à jour toute seule.');
    $('stats').hidden = true;
    text('hint', '');
    schedulePoll(30);
  }

  function showStatus(st) {
    lastStatusAt = Date.now();
    if (st.state === 'closed') { showClosed(st.opens_at); return; }
    $('stats').hidden = false;
    if (st.position > 0) {
      setState('waiting');
      text('eyebrow', 'Vous êtes dans la file');
      text('title', st.position === 1 ? 'Vous êtes le prochain' : 'Votre place est réservée');
      text('lede', 'Vous serez redirigé automatiquement dès que c\'est votre tour. Laissez cet onglet ouvert.');
      text('position', fmtN(st.position));
      text('ahead', fmtN(st.ahead));
      text('eta', fmtEta(st.eta_s));
      const total = Math.max(st.pending, st.position);
      $('fill').style.width = Math.max(4, Math.round((1 - st.ahead / total) * 100)) + '%';
      text('hint', fmtN(st.pending) + ' personnes en attente · ' + (st.rate >= 1 ? Math.round(st.rate) + ' admissions par seconde' : 'admissions en cours'));
    } else {
      setState('wave');
      text('eyebrow', 'Attribution de votre place');
      text('title', 'Votre place sera attribuée dans ' + (st.wave_in_s > 0 ? st.wave_in_s + ' s' : 'quelques instants'));
      text('lede', 'Les arrivées de ces dernières secondes sont mélangées avant d\'entrer dans la file : rafraîchir ou ouvrir d\'autres onglets ne donne aucun avantage.');
      text('position', '–');
      text('ahead', fmtN(st.pending));
      text('eta', fmtEta(st.eta_s));
      text('hint', fmtN(st.pending) + ' personnes déjà en file');
    }
  }

  async function admittedIframe(goPath) {
    if (redirecting) return;
    redirecting = true;
    stopAll();
    if (!parentOrigin) return fail('Intégration invalide', 'La page hôte ne correspond pas au domaine protégé.', false);
    try {
      const r = await fetch(base + '/pass?t=' + encodeURIComponent(ticket), { cache: 'no-store' });
      if (!r.ok) throw new Error('pass indisponible');
      const j = await r.json();
      setState('admitted');
      text('eyebrow', 'C\'est à vous');
      text('title', 'Vous pouvez continuer');
      text('lede', 'Vous êtes admis, la page va se rafraîchir.');
      $('stats').hidden = true;
      window.parent.postMessage({ type: 'flyc:admitted', pass: j.pass, exp: j.exp, dom: j.dom, room: j.room }, parentOrigin);
    } catch (e) { fail('Pass indisponible', 'Réessayez dans quelques secondes.', true); redirecting = false; }
  }

  function admitted(goPath) {
    if (iframeMode) return admittedIframe(goPath);
    if (redirecting) return;
    redirecting = true;
    stopAll();
    setState('admitted');
    text('eyebrow', 'C\'est à vous');
    text('title', 'Vous pouvez accéder au site');
    text('lede', 'Votre pass est valable quelques minutes. Si la redirection ne se fait pas, utilisez le bouton.');
    text('hint', '');
    $('stats').hidden = true;
    $('go').href = goPath;
    setTimeout(() => { location.replace(goPath); }, 400);
  }

  function stopAll() {
    if (es) { es.close(); es = null; }
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
  }

  function schedulePoll(sec) {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = setTimeout(pollStatus, (sec || pollEvery) * 1000);
  }

  async function pollStatus() {
    if (!ticket) return;
    try {
      const r = await fetch(base + '/status?t=' + encodeURIComponent(ticket), { cache: 'no-store' });
      if (r.status === 404) { ticket = null; return start(); }
      const j = await r.json();
      if (j.state === 'admitted') return admitted(j.go);
      if (j.state === 'closed') return showClosed(j.opens_at);
      if (j.status) showStatus(j.status);
      pollEvery = j.poll_s || pollEvery;
    } catch (e) { /* réseau : on réessaie */ }
    schedulePoll();
  }

  function openStream() {
    if (!window.EventSource) return schedulePoll(1);
    es = new EventSource(base + '/events?t=' + encodeURIComponent(ticket));
    es.addEventListener('status', (e) => showStatus(JSON.parse(e.data)));
    es.addEventListener('admitted', (e) => admitted(JSON.parse(e.data).go));
    es.addEventListener('closed', (e) => { stopAll(); showClosed(JSON.parse(e.data).opens_at); });
    es.addEventListener('refresh', () => pollStatus());
    es.onerror = () => {
      // LB en mode dégradé (503) ou coupure : on passe en polling, le navigateur retentera le flux plus tard.
      es.close(); es = null;
      pollEvery = 15;
      schedulePoll(3);
      setTimeout(() => { if (!redirecting && ticket && !es) openStream(); }, 60000);
    };
    // filet de sécurité : si aucun statut depuis 40 s, on interroge
    setInterval(() => { if (es && Date.now() - lastStatusAt > 40000) pollStatus(); }, 20000);
  }

  function applyTheme(title, theme) {
    theme = theme || {};
    if (title) document.title = title + ' · Salle d\'attente';
    if (theme.accent && /^#[0-9a-fA-F]{3,8}$/.test(theme.accent)) document.documentElement.style.setProperty('--accent', theme.accent);
    if (theme.logo_url && /^https:\/\//.test(theme.logo_url)) {
      const img = document.createElement('img'); img.src = theme.logo_url; img.alt = title || ''; img.className = 'logo-img';
      const logo = document.querySelector('.brand .logo'); logo.replaceWith(img);
    } else if (title) { document.querySelector('.brand .logo').textContent = title; }
    if (theme.message) text('hint', theme.message);
  }

  async function start() {
    if (!room) return fail('Lien incomplet', 'Cette page doit être ouverte depuis le site que vous souhaitez visiter.', false);
    text('site', host);
    try { const c = await (await fetch(base + '/config', { cache: 'no-store' })).json(); applyTheme(c.title, c.theme); } catch (e) { /* thème par défaut */ }
    if (closedHint) showClosed(0);
    else setState('loading');
    try {
      const r = await fetch(base + '/ticket', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ h: host, r: back }), cache: 'no-store' });
      const j = await r.json();
      if (r.status === 404) return fail('File d\'attente introuvable', 'Ce lien ne correspond à aucune file active.', false);
      if (r.status === 400) return fail('Lien invalide', j.message || 'Le domaine demandé n\'est pas protégé par cette file.', false);
      if (r.status === 403) return fail('Vérification requise', j.message || 'La vérification anti-robot a échoué.', true);
      if (!r.ok) return fail('Service momentanément indisponible', 'Réessayez dans quelques secondes.', true);
      if (j.state === 'closed') return showClosed(j.opens_at);
      ticket = j.ticket;
      text('ticket-id', 'ticket ' + ticket.slice(2, 10));
      if (j.state === 'admitted') return admitted(j.go);
      if (j.status) showStatus(j.status);
      openStream();
    } catch (e) {
      fail('Connexion impossible', 'Vérifiez votre connexion puis réessayez.', true);
    }
  }

  $('retry').addEventListener('click', () => { stopAll(); redirecting = false; start(); });
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible' && ticket && !redirecting) pollStatus(); });
  start();
})();
