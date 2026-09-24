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

## Infrastructure (portée plateforme)

L'inventaire vit en base : ces routes sont le seul chemin pour le modifier, et l'interface ne fait
rien d'autre que les appeler. Le fichier YAML joué par Ansible en est la projection.

| Méthode | Route | Corps | Effet |
|---|---|---|---|
| GET | `/v1/nodes` | | les nœuds déclarés, avec leurs rôles et leur état |
| POST | `/v1/nodes` | `{name, roles:[edge\|queue\|data], mgmt_ip, mgmt_ip6?, state?, note?}` | déclare un nœud (`planned` par défaut) ; ne l'installe pas |
| PATCH | `/v1/nodes/{name}` | `{mgmt_ip?, mgmt_ip6?, state?, note?}` | champs omis inchangés |
| DELETE | `/v1/nodes/{name}` | | sort le nœud de l'inventaire ; refuse un nœud `ready` ou l'hôte `data` |
| GET | `/v1/settings` | | réglages de plateforme (VIP, BGP, ACME, registre, version des images…) |
| PUT | `/v1/settings` | objet partiel | champs omis inchangés |
| GET | `/v1/inventory` | | l'inventaire YAML tel qu'il sera joué — pièce d'audit |
| GET | `/v1/keys` | | clés de signature des pass |
| POST | `/v1/keys/rotate` | | nouvelle clé active ; l'ancienne reste acceptée 24 h |
| DELETE | `/v1/edges/{name}`, `/v1/queue-hosts/{name}` | | retire une ligne d'état orpheline (dérivée de `nodes`) |

**États d'un nœud** : `planned` (déclaré, pas installé) → `installing` → `ready` (en service) →
`draining` (en retrait). Seuls les nœuds `ready` reçoivent de la configuration ; un nœud `draining`
reste dans l'inventaire — c'est par lui qu'on le démonte — mais les plays d'installation
l'excluent.

### Enrôlement d'une machine

| Méthode | Route | Corps | Effet |
|---|---|---|---|
| POST | `/v1/enroll/tokens` | `{roles?}` | jeton à usage unique, 15 min ; renvoie `{id, url, command, expires_at}` |
| GET | `/v1/enroll/tokens` | | jetons en cours et machines déjà annoncées |
| DELETE | `/v1/enroll/tokens/{id}` | | révoque un jeton |
| GET | `/enroll/{token}` | **sans authentification** | rend le script à exécuter sur la machine |
| POST | `/enroll/{token}/done` | `{hostname, mgmt_ip, mgmt_ip6}`, **sans authentification** | consomme le jeton et annonce la machine |

Le jeton est la seule preuve : la machine à enrôler n'a par définition aucun compte. Rien de secret
ne circule — le script ne transporte qu'une clé **publique**. Le jeton n'est stocké que haché, et un
rejeu renvoie 404.

### Tâches

Les opérations d'infrastructure ne sont pas exécutées par flyc-control mais par le conteneur
`flyc-runner`, à qui il dépose une demande. Une seule tâche à la fois.

| Méthode | Route | Corps | Effet |
|---|---|---|---|
| POST | `/v1/jobs` | `{kind, request:{kind: playbook\|script, playbook\|argv, limit?, tags?, extra_vars?, check?}}` | dépose une tâche ; 409 `busy` si une autre tourne, 503 si aucun runner |
| GET | `/v1/jobs?limit=N` | | historique |
| GET | `/v1/jobs/{id}` | | état : `pending`, `running`, `succeeded`, `failed`, `interrupted` |
| GET | `/v1/jobs/{id}/log` | | journal en direct (SSE), événement `end` à la fin |

L'inventaire est généré depuis la base et joint automatiquement : l'appelant n'a rien à fournir, et
ne peut donc pas faire exécuter une topologie qui n'est pas celle déclarée. Les points d'entrée sont
sur liste blanche côté runner (`edge/site.yml`, `tools/propagate-node.yml`, `tools/redis-detach.yml`,
`tools/decommission-node.yml`, `tools/control-api.yml`, `tools/add-node.sh`, `tools/check-node.sh`).

### Ajouter un hôte de file, de bout en bout

C'est exactement la séquence que déclenche le bouton « Installer » de l'interface.

```sh
K=<clé plateforme>; C=http://10.0.0.65:8090

# 1. un jeton, puis la ligne à coller sur la machine à enrôler
curl -s -X POST -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
     -d '{"roles":["queue"]}' $C/v1/enroll/tokens        # → {"command":"curl -fsSL … | sudo sh"}

# 2. une fois la machine annoncée, on la déclare (elle n'est pas encore installée)
curl -s -X POST -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
     -d '{"name":"queue3","roles":["queue"],"mgmt_ip":"10.0.0.13","state":"planned"}' $C/v1/nodes

# 3. installation : la passe converge aussi les autres nœuds, dont les règles de pare-feu
#    énumèrent les adresses autorisées
curl -s -X POST -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
     -d '{"kind":"infra","request":{"kind":"playbook","playbook":"edge/site.yml"}}' $C/v1/jobs

# 4. déclaration sur les LB déjà en service, par la Dataplane API
curl -s -X POST -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
     -d '{"kind":"infra","request":{"kind":"playbook","playbook":"tools/propagate-node.yml",
          "extra_vars":{"propagate_kind":"queue","propagate_name":"queue3",
                        "propagate_ip":"10.0.0.13","propagate_state":"present",
                        "propagate_targets":"lb0,lb1"}}}' $C/v1/jobs

# 5. mise en service, puis vérification
curl -s -X PATCH -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
     -d '{"state":"ready"}' $C/v1/nodes/queue3
curl -s -X POST  -H "Authorization: Bearer $K" $C/v1/sync
curl -s -X POST  -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
     -d '{"kind":"infra","request":{"kind":"script","argv":["tools/check-node.sh","queue","queue3"]}}' $C/v1/jobs
```

Chaque étape est une tâche : suivre son journal avec `GET /v1/jobs/{id}/log`, et attendre qu'elle
soit `succeeded` avant la suivante — une seule tâche tourne à la fois.

Le retrait suit le chemin inverse : `PATCH state=draining`, `tools/redis-detach.yml` pour un hôte
de file, `tools/propagate-node.yml` avec `propagate_state=absent`, `tools/decommission-node.yml`
limité au nœud, `DELETE /v1/nodes/{name}`, puis une convergence des nœuds restants.

## Comptes et authentification

| Méthode | Route | Corps | Effet |
|---|---|---|---|
| POST | `/auth/login` | `{email, password, totp?}` | ouvre une session (cookie) ; `GET /auth/config` dit si OIDC est actif |
| POST | `/auth/logout` | | |
| GET | `/v1/account` | | compte courant, appartenances, `must_change_password` |
| POST | `/v1/account/password` | `{current, new}` | |
| POST | `/v1/account/totp/setup` | | prépare le double facteur |
| POST | `/v1/account/totp/enable` | `{code}` | active après vérification d'un code |
| POST | `/v1/account/totp/disable` | `{password}` | désactive |
| GET | `/v1/account/totp/qr.png` | | QR code d'appairage |
| GET | `/auth/config` | | indique si OIDC est actif (page de connexion) |
| GET | `/auth/oidc/start` | | démarre le flux OIDC |
| GET | `/auth/oidc/callback` | | retour du fournisseur d'identité |
| GET/POST | `/v1/users` | `{email, password, platform_admin?}` | comptes (plateforme) |
| POST | `/v1/users/{id}/password`, DELETE `/v1/users/{id}` | | |
| GET/PUT | `/v1/tenants/{slug}/members` | `{email, role: viewer\|operator\|admin}` | appartenances |
| DELETE | `/v1/tenants/{slug}/members/{email}` | | |

## Routes internes

`GET /internal/signing-key` est réservée aux composants de la plateforme : flyc-queue s'en sert pour
récupérer la clé de signature courante, avec le jeton interne partagé (`FLYC_INTERNAL_TOKEN`). Elle
n'est pas destinée aux intégrateurs et n'est pas exposée par la VIP.

## Routes publiques

| Méthode | Route | Effet |
|---|---|---|
| GET | `/.well-known/jwks.json` | clés publiques de vérification des pass |
| GET | `/.well-known/flyc-pass.pem` | même chose au format PEM |
| GET | `/enroll/{token}` | script d'enrôlement (le jeton fait l'authentification) |

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
| POST | `/v1/pass/verify` | `{pass}` | vérifie un pass côté serveur (signature, expiration, domaine) sans dépendre du JWKS |
| GET/POST | `/v1/api-keys` | `{name}` | clés API du tenant ; la valeur n'est rendue qu'à la création |
| DELETE | `/v1/api-keys/{id}` | | révoque une clé |

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
