# Plan directeur

Architecture, pass ES256, admission par vagues, Redis Cluster, control plane
multi-tenant, déploiement. Les procédures d'exploitation sont dans
`docs/runbooks.md`, l'API dans `docs/api.md`.

Décisions figées :

* **Redis Cluster** (3 masters + 3 replicas) dès le départ, hash tags `{r:<tenant>:<room>}`.
* **Admission par vagues aléatoires** : fenêtres de 30 s, ordre tiré au sort à la
  fermeture, vagues ordonnées entre elles.
* **Multi-tenant** natif : tenants, rôles, isolation dans le schéma et les jetons.
* **Docker Compose partout hors edge**, aucun Kubernetes, aucun coffre de secrets.
* **Pass** = JWT ES256 vérifié par HAProxy (`jwt_verify`) sans appel réseau.
* Les **backends du client** ne sont jamais installés ni touchés par Ansible.

## Jalons

| Jalon | Contenu | Fini quand |
|-------|---------|------------|
| M0 ✓ | dépôt, rôles Ansible, compose, socle Go, pipeline | fait le 20/09/2026 |
| M1 ✓ | pass ES256, maps, logique HAProxy, page d'attente minimale | fait le 20/09/2026 |
| M2 ✓ | flyc-queue : tickets, vagues, admitter, SSE, signature | fait le 20/09/2026 |
| M3 ✓ | flyc-control : schéma, API, compilation, push Dataplane, ACME | fait le 20/09/2026, voir docs/api.md |
| M4 ✓ | UI web, comptes locaux + TOTP, OIDC optionnel | fait le 20/09/2026, UI embarquée dans flyc-control |
| M5 ✓ | widget iframe/JS, JWKS, SDK, mode auto | fait le 20/09/2026, voir docs/integration.md |
| M6a ✓ | charge 100 000 visiteurs, deux sources | fait le 20/09/2026 |
| M6b ✓ | supervision (Prometheus, Alertmanager, Grafana), rotation de clés | fait le 21/09/2026 |
| M6c ✓ | runbooks (`docs/runbooks.md`), profil all-in-one validé sur VM vierge (test01, 10.0.0.67) | fait le 21/09/2026 |
| M7 | bascule sur lb0/lb1, vitrine example.com | plus tard |
