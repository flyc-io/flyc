# Flyc

Salle d'attente virtuelle self-hostée pour billetteries et événements.

Un seul dépôt déploie, sans intervention manuelle, une stack complète sur un ou
plusieurs load balancers anycast (HAProxy + BIRD) et sur des hôtes Docker qui
font tourner la file d'attente (flyc-queue + Redis Cluster) et le control plane
(flyc-control + Postgres).

Plan directeur complet : voir `docs/blueprint.md`.

## Briques

| Brique         | Rôle                                                                | Où                                   |
|----------------|---------------------------------------------------------------------|--------------------------------------|
| `flyc-edge`    | HAProxy 3.0, BIRD 2, Dataplane API : TLS, vérification du pass, BGP | hôtes `edge`, installés par Ansible  |
| `flyc-queue`   | tickets, vagues, admission, SSE, signature des pass                 | hôtes `queue`, Docker Compose        |
| `flyc-control` | API, UI, compilation et push de la config edge, ACME                | hôte `data`, Docker Compose          |
| `flyc-wait`    | page de salle d'attente et widget, servie par HAProxy               | fichiers déposés sur les hôtes `edge`|

Les **backends** (les serveurs qui rendent le site du client) ne font pas
partie du déploiement : ils sont déclarés dans l'interface comme adresse et
port, Ansible ne s'y connecte jamais.

## Prérequis

* Des machines Ubuntu 24.04 vierges, accessibles en SSH avec un utilisateur sudo.
* Pour les hôtes `edge` : une ou deux IP à annoncer en BGP (ou une IP fixe sans
  BGP pour un LB unique).
* Un accès sortant vers Docker Hub depuis chaque machine : la pile s'appuie sur des images
  publiques épinglées (postgres, redis, prometheus, alertmanager, grafana).
* Ansible n'est nécessaire **que** pour la variante « depuis un poste » ci-dessous. L'installation
  normale n'en demande pas : c'est l'image `flyc-runner` qui joue les playbooks.

Rien d'autre : ni Kubernetes, ni coffre de secrets, ni image préparée.

## Installer depuis zéro

Sur une machine Ubuntu vierge qui deviendra l'hôte control :

```sh
curl -fsSL https://raw.githubusercontent.com/flyc-io/flyc/main/tools/install-control.sh -o install-control.sh
sudo sh install-control.sh --control-host control.exemple.fr --wait-host wait.exemple.fr
```

Le script pose Docker, le compte de déploiement et sa paire de clés, puis fait jouer l'installation
par l'image `flyc-runner`. Il affiche à la fin l'URL de l'interface, le compte administrateur et la
clé plateforme. Il est rejouable : relancer la même commande après un échec reprend où c'était.

Tout le reste — déclarer les LB et les hôtes de file, les installer, les retirer — se fait ensuite
depuis l'interface : Plateforme → Infrastructure. Chaque machine à ajouter est enrôlée par une ligne
`curl … | sudo sh` que l'interface fournit, et qui n'y dépose qu'une clé **publique**.

### D'où viennent les images

Trois images portent Flyc : `flyc-queue`, `flyc-control` et `flyc-runner` (qui embarque les
playbooks, si bien que sa version *est* la version de la plateforme). Elles sont publiées sur
`ghcr.io/flyc-io` à chaque version, sous trois étiquettes : `v1.2.3`, `v1.2` (suit les correctifs
de la série) et `latest` (dernière version publiée, jamais la pointe de `main`).

| Situation | Commande |
|---|---|
| Cas normal | rien à faire, `latest` est le défaut |
| Version épinglée | `--image-tag v1.2.3` |
| Miroir interne | `--registry registre.interne.example/flyc` (`--registry-ca` si CA privée) |
| Rien tirer de l'extérieur | `--from-source`, voir ci-dessous |

### Sans registre public : `--from-source`

Pour un site qui ne tire rien d'Internet, ou qui veut maîtriser ce qu'il exécute :

```sh
git clone <dépôt> && cd flyc
sudo bash tools/install-control.sh --from-source --control-host … --wait-host …
```

L'hôte control construit alors les trois images depuis les sources, génère un certificat, lance un
registre minimal (`registry:2`) sur son adresse `:5000`, y publie les images, et inscrit le
registre **et son certificat** dans les réglages de la plateforme. Les nœuds ajoutés ensuite
reçoivent ce certificat par l'inventaire généré et tirent depuis control : aucune image Flyc ne
transite par Internet. Les images tierces (postgres, redis, supervision) restent à mirrorer
séparément pour un site totalement coupé.

### Mettre à jour

La version déployée est un **réglage de la plateforme**, pas une valeur de fichier : Plateforme →
Infrastructure → Réglages, ou `PUT /v1/settings {"image_tag":"v1.2.4"}`, puis « Déployer la
plateforme ». Voir `docs/runbooks.md`.

## Démarrage rapide

```sh
tools/setup.sh mine          # assistant : pose les questions, vérifie SSH, écrit inventory/mine.yml
ansible-galaxy collection install -r edge/requirements.yml
ansible-playbook -i inventory/mine.yml edge/site.yml
```

Sans l'assistant, copier `inventory/example-all-in-one.yml` (ou `test.yml`) et
remplir les champs marqués « À RENSEIGNER ». Aucun secret n'est saisi : ils sont
générés sur l'hôte `data` au premier run. En fin de run, le playbook affiche le
compte initial de l'interface d'administration (`admin@localhost` et son mot de
passe, aussi dans `/var/lib/flyc/control/initial-admin-password.txt`).

Le playbook installe tous les paquets, génère les secrets sur l'hôte `data`,
déploie le compose sur les hôtes `queue` et `data`, puis configure les LB en
série avec vérification BGP et health check après chacun.

### Sur une seule machine

Le profil all-in-one installe tout sur une machine Ubuntu 24.04 vierge (4 vCPU, 8 Go suffisent
pour essayer) : HAProxy écoute directement sur l'IP de la machine, les six nœuds Redis Cluster,
flyc-queue, flyc-control, Postgres et la supervision tournent dessus. Répondre « oui » à la
première question de l'assistant, ou partir de `inventory/example-all-in-one.yml` :

```sh
tools/setup.sh solo           # « Installation sur une seule machine ? » → oui, puis l'IP de la machine
ansible-playbook -i inventory/solo.yml edge/site.yml
```

Le playbook détecte le profil (un seul hôte `queue`) : pas de BGP, pas de VIP sur `lo`, six Redis
sur 7000-7005, règles de firewall pour les conteneurs qui joignent l'hôte. Les noms `flyc_wait_host`
et `flyc_control_host` doivent pointer sur l'IP de la machine ; avec `flyc_acme_email` renseigné,
l'interface d'administration est servie en HTTPS par le LB sur `https://<flyc_control_host>`.
Sans redondance : pour la production, séparer les rôles (2 LB, 3 hôtes queue, 1 control).
Validé sur une VM vierge (Ubuntu 24.04, 8 vCPU, 16 Go) : le playbook aboutit en un run, le second run ne change rien,
le parcours complet (ticket, admission, pass, page protégée) passe par le LB unique.

## Arborescence

```
inventory/        inventaires d'amorçage (test, production, all-in-one) ; ensuite la base fait foi
edge/             Ansible : playbook site.yml et rôles
queue/            flyc-queue (Go)
control/          flyc-control (Go) + migrations SQL
internal/         paquets Go partagés
wait/             page de salle d'attente + widget (statique)
deploy/compose/   docker-compose.yml (profils queue, data, all-in-one) et conf Redis
loadtest/         scénarios k6
docs/             plan directeur, runbooks
tools/            scripts de rendu et de validation (haproxy -c)
```

## Ajouter un nœud à une installation en service

`tools/add-node.sh` fait les quatre gestes d'un ajout : il écrit l'hôte dans l'inventaire, rejoue le
playbook, déclare le nouveau nœud sur les LB déjà en place via la Dataplane API, et l'inscrit dans
la base de flyc-control — qui fait foi pour l'inventaire (voir « L'inventaire vit en base » dans les
runbooks).

```sh
tools/add-node.sh lb    lb2    203.0.113.12 2001:db8::12   # nouveau load balancer
tools/add-node.sh queue queue3 10.0.0.13                   # nouvel hôte de file
tools/add-node.sh backend acme boutique 10.0.0.20:80 10.0.0.21:80   # backend d'un client
tools/add-node.sh list                                     # état de l'inventaire
tools/add-node.sh remove queue queue3                      # retrait : sortie du cluster Redis
                                                           # puis mise hors service du nœud
tools/add-node.sh --purge remove lb lb2                    # retrait + machine remise à zéro
```

`--check` décrit l'opération sans rien modifier, `-y` enchaîne sans confirmation, `-i` désigne un
autre inventaire et `-t` épingle le tag d'image.

L'arrivée d'un nœud change aussi l'état attendu des autres : leurs règles de pare-feu énumèrent les
adresses autorisées, et les hôtes de file portent la liste des nœuds. Le script leur applique donc
d'abord ce qui les concerne (`--tags firewall`, plus `compose` pour un hôte de file), puis installe
le nouveau nœud. Rejouer tout le playbook partout donnerait le même résultat mais coûterait
plusieurs minutes pour deux tâches utiles ; `--full` le fait si on y tient.

Ce qui se passe selon le type :

* **Load balancer** : installation complète (HAProxy, BIRD, Dataplane API), annonce des mêmes VIP en
  BGP, puis flyc-control lui pousse backends, maps et certificats. Les LB déjà en service reçoivent
  une entrée dans leur section `peers` (stick-table de limitation partagée) sans réécriture de leur
  configuration et sans coupure.
* **Hôte de file** : ses deux nœuds Redis rejoignent le cluster existant, un maître qui reçoit des
  slots par rééquilibrage et une réplique placée sur un autre hôte que son maître. Il est ensuite
  ajouté comme serveur de `bk_flyc_queue` sur chaque LB. Opération en ligne, à faire hors événement :
  flyc-queue redémarre sur les hôtes existants, les flux SSE se reconnectent et les visiteurs gardent
  leur place.
* **Backend** : simple déclaration dans flyc-control. Les serveurs appartiennent au client, Flyc ne
  les installe jamais.

Un retrait ne se contente pas de sortir le nœud de l'inventaire : ses services sont arrêtés **et
désactivés** et ses VIP retirées, sinon un redémarrage le ferait réapparaître dans le pool avec une
configuration figée. `--purge` va plus loin et rend la machine vierge. Sa ligne est aussi supprimée
dans flyc-control, pour que l'interface ne garde pas de nœud fantôme.

Le script se termine par `tools/check-node.sh`, qui contrôle le nœud et sa visibilité depuis
flyc-control et depuis chaque LB. On peut le rejouer seul à tout moment.

### Durée d'un déploiement

Un passage à vide sur huit hôtes prend environ trois minutes, l'ajout d'un load balancer à une
plateforme en service environ six, dont l'essentiel est le téléchargement des paquets sur la
nouvelle machine. Les LB déjà en place continuent de servir pendant toute l'opération.

`make ansible-fast` installe Mitogen dans le dépôt (le Python du système n'est pas touché) : les
outils l'utilisent alors automatiquement et le passage à vide tombe sous la minute et demie. Rien
n'en dépend, son absence ne change que la durée. Pour en profiter à la main :
`source tools/ansible-env.sh` avant `ansible-playbook`.

## Après l'installation

La configuration des clients (domaines, backends, files) se fait dans l'interface web de
flyc-control (`http://<control>:8090`, compte `admin@localhost`, mot de passe dans
`/var/lib/flyc/control/initial-admin-password.txt`, à changer à la première connexion) ou par
son API : voir `docs/api.md`. OIDC optionnel via `flyc_oidc` dans l'inventaire. La clé plateforme initiale est dans `/var/lib/flyc/control/initial-platform-api-key.txt`
sur l'hôte control. Les certificats sont obtenus automatiquement par ACME si `flyc_acme_email` est renseigné.

## Exploitation

Les procédures (déploiement, jour J, LB désynchronisé, perte d'une replica ou d'un hôte, certificats,
rotation, sauvegardes, mode dégradé) sont dans `docs/runbooks.md`.

## Supervision

Le profil compose `monitoring` (activé par défaut sur l'hôte `data`) démarre Prometheus, Alertmanager
et Grafana préconfigurés : cibles HAProxy, flyc-queue, flyc-control et node_exporter de tous les hôtes,
douze règles d'alerte (LB ou hôte queue injoignable, LB désynchronisé, file sans admitter, certificat
en échec ou proche de l'expiration, serveur backend DOWN, mode dégradé, mémoire), un tableau de bord
« Flyc · plateforme ». Grafana écoute sur `http://<hôte data>:3000` (admin, mot de passe dans
`secrets.env`). Renseigner `flyc_alert_webhook_url` dans l'inventaire pour recevoir les alertes.

## Rotation des clés de signature

`POST /v1/keys/rotate` (ou le bouton de la page Infrastructure) active une nouvelle clé ES256.
L'ancienne reste acceptée 24 h par les LB, le JWKS et `/v1/pass/verify`, le temps que les pass en
cours expirent. Les LB sont rechargés automatiquement ; flyc-queue signe avec la nouvelle clé en
moins d'une minute.

## Développement

```sh
make test            # go vet + go test
make check-haproxy   # rend la config HAProxy depuis un inventaire fixture et lance haproxy -c
make images          # build local des images
```

## Licence

AGPL-3.0 — voir `LICENSE`. Vous pouvez l'utiliser, le modifier et l'héberger librement ; si vous en
proposez un service à des tiers, les modifications doivent être publiées sous la même licence.

Contributions bienvenues : voir `CONTRIBUTING.md`.
