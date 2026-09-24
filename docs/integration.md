# Intégrer Flyc à un site

## Mode DNS (recommandé)

1. Créez le domaine dans Flyc (`POST /v1/domains` ou l'interface) en mode `dns`.
2. Pointez-le sur la VIP de la plateforme (A/AAAA ou CNAME). Le certificat est obtenu automatiquement.
3. Créez un backend avec vos serveurs et une file qui relie domaine et backend.

Tout le trafic traverse Flyc : les visiteurs sans pass sont mis en attente avant même d'atteindre
vos serveurs. Les requêtes admises portent les en-têtes `X-Flyc-Room` et `X-Flyc-Pass-Sub`.

## Mode iframe / JS (vous gardez votre DNS)

1. Créez le domaine en mode `iframe` (pas de certificat, pas de routage côté LB) et rattachez-le à une file.
2. Sur les pages à protéger :
   ```html
   <script src="https://wait.example.com/widget.js" data-room="acme.concert"></script>
   ```
3. Tant que le visiteur n'a pas de pass valide, le widget recouvre la page d'une salle d'attente. À
   l'admission, il pose le cookie `flyc_pass` sur votre domaine, expose `window.Flyc.pass`, émet
   l'événement `flyc:pass` et recharge la page (`data-reload="false"` pour le désactiver).
4. **Vérifiez le pass côté serveur** sur chaque requête sensible : SDK Node (`sdk/node`), SDK PHP
   (`sdk/php`), ou `POST /v1/pass/verify` avec votre clé API. Le widget seul n'est pas une protection.

Ce mode protège l'expérience utilisateur, pas vos serveurs : la page initiale les atteint. Seul le
mode DNS absorbe la charge.

## Mode automatique

Une file en état `auto` s'active seule : Flyc mesure les sessions courantes vers le backend sur tous
les LB toutes les 5 secondes. Au-dessus de `auto_on` connexions, la file passe en `queue` ; en dessous
de `auto_off`, elle repasse en `open`. Les seuils se règlent dans la file (mode DNS uniquement, le LB
doit voir le trafic).

## Le pass

JWT ES256 signé par flyc-control, claims : `sub` (ticket), `tid` (tenant), `dom` (domaine), `rm`
(file), `iat`, `exp`, `kid`. Clés publiques : `https://<salle d'attente>/.well-known/jwks.json`.
