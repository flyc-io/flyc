# API flyc-control v1

Base : `http://<control>:8090` (ou `https://control.<plateforme>` via la VIP app).
Authentification : `Authorization: Bearer <clé API>`.

* **Clé plateforme** : créée au premier démarrage, dans `/var/lib/flyc/control/initial-platform-api-key.txt`
  sur l'hôte control. Agit sur un tenant en ajoutant `X-Flyc-Tenant: <slug>`.
* **Clé tenant** : rendue une seule fois à la création du tenant ou par `POST /v1/tenants/{slug}/api-keys`.
  Ne voit que son tenant ; `X-Flyc-Tenant` est ignoré.

## Plateforme

| Méthode | Route | Corps | Effet |
|---|---|---|---|
| GET | `/v1/me` | | qui suis-je |
| POST | `/v1/tenants` | `{slug, name}` | crée le tenant et renvoie sa première clé API |
| GET | `/v1/tenants` | | liste |
| POST | `/v1/tenants/{slug}/api-keys` | `{name}` | nouvelle clé tenant |
| GET | `/v1/edges` | | état de synchronisation de chaque LB |
| GET | `/v1/queue-hosts` | | santé des hôtes queue |
| POST | `/v1/sync` | | réconciliation immédiate + passe ACME |

## Tenant

| Méthode | Route | Corps | Effet |
|---|---|---|---|
| GET/POST | `/v1/domains` | `{fqdn, mode: dns\|iframe, tls: auto\|none}` | domaines ; `tls: auto` déclenche l'ACME |
| DELETE | `/v1/domains/{fqdn}` | | |
| GET | `/v1/domains/{fqdn}/certificate` | | statut, émetteur, expiration, dernière erreur |
| POST | `/v1/domains/{fqdn}/certificate/renew` | | réémission immédiate |
| GET/POST | `/v1/backends` | `{name, balance, check_path, servers:[{name,address,port,weight,maxconn}]}` | backends du client (jamais installés par Flyc) |
| PUT/DELETE | `/v1/backends/{name}` | | |
| GET/POST | `/v1/rooms` | `{slug, title, domain, backend, state, rate, max_active, wave_s, pass_ttl_s, opens_at}` | files |
| PUT/DELETE | `/v1/rooms/{slug}` | | |
| POST | `/v1/rooms/{slug}/state` | `{state: open\|queue\|closed}` | bascule à chaud ; la réponse arrive après la mise à jour des LB |
| GET | `/v1/rooms/{slug}/stats` | | en attente, actifs, vague en cours, compteurs |
| POST | `/v1/rooms/{slug}/flush` | | vide la file |

## Ce qui se passe après un appel

1. Chaque écriture incrémente `config_version` ; le réconciliateur recompile l'état désiré.
2. Sur chaque LB, via la Dataplane API : backends en transaction (reload seulement si un backend
   ou un serveur change), maps `rooms` / `states` / `backends` par différence à chaud,
   certificats déposés et crt-lists mises à jour (reload).
3. flyc-queue reçoit la configuration de chaque file (`PUT /internal/rooms/<tenant>.<room>`).
4. `GET /v1/edges` indique `in_sync: true` quand le hash déployé est celui de l'état désiré.

Un LB injoignable n'empêche pas les autres d'être mis à jour ; il est repris à la passe suivante
(toutes les 60 s, ou immédiatement à la prochaine écriture).

## Exemple complet

```sh
K=<clé plateforme>; C=http://10.0.0.65:8090
curl -s -X POST $C/v1/tenants -H "Authorization: Bearer $K" -d '{"slug":"acme","name":"ACME Billetterie"}'
curl -s -X POST $C/v1/domains  -H "Authorization: Bearer $K" -H "X-Flyc-Tenant: acme" -d '{"fqdn":"billets.acme.fr"}'
curl -s -X POST $C/v1/backends -H "Authorization: Bearer $K" -H "X-Flyc-Tenant: acme" \
  -d '{"name":"shop","check_path":"/health","servers":[{"address":"10.0.0.10","port":80},{"address":"10.0.0.11","port":80}]}'
curl -s -X POST $C/v1/rooms -H "Authorization: Bearer $K" -H "X-Flyc-Tenant: acme" \
  -d '{"slug":"concert","title":"Concert","domain":"billets.acme.fr","backend":"shop","state":"queue","rate":50,"wave_s":30}'
curl -s -X POST $C/v1/rooms/concert/state -H "Authorization: Bearer $K" -H "X-Flyc-Tenant: acme" -d '{"state":"open"}'
```

`tools/edge-domain.sh` reste disponible comme outil de secours (Dataplane API directe) mais
n'est plus nécessaire : flyc-control est la source de vérité.
