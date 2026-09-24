/* Flyc — widget iframe / JS.
   <script src="https://wait.example/widget.js" data-room="tenant.room"></script>
   Sans pass valide : recouvre la page d'un cadre « salle d'attente ». À l'admission : reçoit le pass par
   postMessage, le stocke (cookie flyc_pass sur le domaine du site + localStorage), retire le cadre,
   émet l'événement `flyc:pass` et expose window.Flyc.pass. Le site vérifie le pass côté serveur
   (SDK ou POST /v1/pass/verify). Aucune dépendance. */
(function () {
  'use strict';
  const s = document.currentScript;
  if (!s || !s.dataset.room) return;
  const room = s.dataset.room;
  const waitOrigin = new URL(s.src).origin;
  const host = (s.dataset.host || location.hostname).toLowerCase();
  const key = 'flyc_pass_' + room;
  const cookieName = 'flyc_pass';
  const grace = 30; // secondes de marge avant expiration

  const Flyc = window.Flyc = window.Flyc || {};
  Flyc.room = room;
  Flyc.pass = null;

  function decodeExp(jwt) {
    try { const p = JSON.parse(atob(jwt.split('.')[1].replace(/-/g, '+').replace(/_/g, '/'))); return { exp: p.exp || 0, dom: p.dom || '', rm: p.rm || '' }; } catch (e) { return null; }
  }
  function valid(jwt) {
    const c = jwt && decodeExp(jwt);
    return !!(c && c.exp > Math.floor(Date.now() / 1000) + grace && c.dom === host && c.rm === room);
  }
  function store(jwt, exp) {
    try { localStorage.setItem(key, jwt); } catch (e) { /* stockage indisponible */ }
    const maxAge = Math.max(0, exp - Math.floor(Date.now() / 1000));
    document.cookie = cookieName + '=' + jwt + '; Path=/; Max-Age=' + maxAge + '; SameSite=Lax' + (location.protocol === 'https:' ? '; Secure' : '');
  }
  function load() {
    try { const v = localStorage.getItem(key); if (valid(v)) return v; } catch (e) { /* ignore */ }
    const m = document.cookie.match(new RegExp('(?:^|; )' + cookieName + '=([^;]+)'));
    if (m && valid(m[1])) return m[1];
    return null;
  }
  function announce(jwt) {
    Flyc.pass = jwt;
    Flyc.claims = decodeExp(jwt);
    document.dispatchEvent(new CustomEvent('flyc:pass', { detail: { pass: jwt, claims: Flyc.claims } }));
  }

  const existing = load();
  if (existing) { announce(existing); return; }

  // Salle d'attente en surimpression
  const overlay = document.createElement('div');
  overlay.id = 'flyc-overlay';
  overlay.setAttribute('style', 'position:fixed;inset:0;z-index:2147483647;background:#0f1418;');
  const frame = document.createElement('iframe');
  frame.title = 'Salle d\'attente';
  frame.setAttribute('style', 'border:0;width:100%;height:100%;display:block;');
  frame.setAttribute('allow', '');
  frame.src = waitOrigin + '/w/' + encodeURIComponent(room) + '?mode=iframe&h=' + encodeURIComponent(host) + '&o=' + encodeURIComponent(location.origin) + '&r=' + encodeURIComponent(btoa(location.pathname + location.search));
  overlay.appendChild(frame);
  const mount = () => { document.documentElement.appendChild(overlay); document.documentElement.style.overflow = 'hidden'; };
  if (document.documentElement) mount(); else document.addEventListener('DOMContentLoaded', mount);

  window.addEventListener('message', (ev) => {
    if (ev.origin !== waitOrigin || !ev.data || typeof ev.data !== 'object') return;
    if (ev.data.type === 'flyc:admitted' && typeof ev.data.pass === 'string') {
      if (!valid(ev.data.pass)) return;
      store(ev.data.pass, ev.data.exp);
      overlay.remove();
      document.documentElement.style.overflow = '';
      announce(ev.data.pass);
      if (s.dataset.reload !== 'false') location.reload(); // le site voit le cookie dès le prochain rendu
    } else if (ev.data.type === 'flyc:closed') {
      document.dispatchEvent(new CustomEvent('flyc:closed', { detail: { opens_at: ev.data.opens_at } }));
    }
  });

  Flyc.reset = function () { try { localStorage.removeItem(key); } catch (e) { /* ignore */ } document.cookie = cookieName + '=; Path=/; Max-Age=0'; };
})();
