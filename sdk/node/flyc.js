'use strict';
// SDK Flyc pour Node.js — vérification des pass (JWT ES256) avec les clés publiques de la plateforme.
// Sans dépendance. Node ≥ 18.
const crypto = require('node:crypto');

class FlycVerifier {
  /**
   * @param {object} opts
   * @param {string} opts.jwksUrl  ex. https://wait.example.com/.well-known/jwks.json
   * @param {string} opts.host     domaine protégé attendu dans le claim dom (ex. billets.acme.fr)
   * @param {string} [opts.room]   file attendue (tenant.room) ; facultatif
   * @param {number} [opts.cacheSeconds=300]
   */
  constructor({ jwksUrl, host, room, cacheSeconds = 300 }) {
    if (!jwksUrl || !host) throw new Error('jwksUrl et host sont requis');
    this.jwksUrl = jwksUrl; this.host = host.toLowerCase(); this.room = room; this.cacheSeconds = cacheSeconds;
    this.keys = new Map(); this.fetchedAt = 0;
  }
  async refresh(force) {
    if (!force && Date.now() - this.fetchedAt < this.cacheSeconds * 1000 && this.keys.size) return;
    const res = await fetch(this.jwksUrl, { headers: { accept: 'application/json' } });
    if (!res.ok) throw new Error('JWKS ' + res.status);
    const { keys } = await res.json();
    this.keys = new Map();
    for (const jwk of keys || []) {
      if (jwk.kty === 'EC' && jwk.crv === 'P-256') this.keys.set(jwk.kid, crypto.createPublicKey({ key: jwk, format: 'jwk' }));
    }
    this.fetchedAt = Date.now();
  }
  /** Vérifie un pass. Renvoie les claims, ou lève une erreur. */
  async verify(token) {
    if (typeof token !== 'string' || token.split('.').length !== 3) throw new Error('format invalide');
    const [h, p, s] = token.split('.');
    const header = JSON.parse(Buffer.from(h, 'base64url'));
    if (header.alg !== 'ES256') throw new Error('algorithme inattendu');
    await this.refresh(false);
    let key = this.keys.get(header.kid);
    if (!key) { await this.refresh(true); key = this.keys.get(header.kid); }
    if (!key) throw new Error('clé inconnue : ' + header.kid);
    const ok = crypto.verify('sha256', Buffer.from(h + '.' + p), { key, dsaEncoding: 'ieee-p1363' }, Buffer.from(s, 'base64url'));
    if (!ok) throw new Error('signature invalide');
    const claims = JSON.parse(Buffer.from(p, 'base64url'));
    const now = Math.floor(Date.now() / 1000);
    if (!claims.exp || claims.exp <= now) throw new Error('pass expiré');
    if ((claims.dom || '').toLowerCase() !== this.host) throw new Error('pass pour un autre domaine');
    if (this.room && claims.rm !== this.room) throw new Error('pass pour une autre file');
    return claims;
  }
  /** Middleware Express : lit le cookie flyc_pass ou l'en-tête X-Flyc-Pass ; 403 sinon. */
  express({ onDenied } = {}) {
    return async (req, res, next) => {
      const cookie = (req.headers.cookie || '').split(/;\s*/).find(c => c.startsWith('flyc_pass='));
      const token = req.headers['x-flyc-pass'] || (cookie ? cookie.slice('flyc_pass='.length) : '');
      try { req.flyc = await this.verify(token); return next(); } catch (e) {
        if (onDenied) return onDenied(req, res, e);
        res.status(403).json({ error: 'flyc_pass_required', message: e.message });
      }
    };
  }
}
module.exports = { FlycVerifier };
