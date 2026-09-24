# Runbooks d'exploitation

Chaque procédure indique où agir, quoi vérifier, et comment revenir en arrière.

## Le compte de déploiement

Control se connecte aux nœuds avec `flyc-deploy`, jamais avec le compte d'administration. La paire
de clés est générée sur l'hôte control au premier déploiement, dans `/var/lib/flyc/deploy/` ; la
privée n'en sort jamais, seule la publique est posée sur les machines.

Il faut être lucide sur ce que ce compte apporte. Configurer une machine suppose d'installer des
paquets et d'écrire dans `/etc` : une liste blanche sudo serait un décor, `apt-get install` suffit à
devenir root. Ce compte est donc root par construction. Ce qu'on gagne est ailleurs, et c'est réel :

* la clé n'est utilisable que depuis l'hôte control (`from=` dans `authorized_keys`), et n'autorise
  ni redirection de port, ni agent, ni X11 ;
* le compte n'a pas de mot de passe, il ne sert qu'à la clé ;
* ses actions sont tracées sous une identité distincte de celle de l'administrateur ;
* une seule rotation de la clé sur control révoque l'accès partout.

`flyc_deploy_from` fixe la ou les adresses autorisées, vide pour ne pas restreindre. Le playbook
réécrit `authorized_keys` à chaque passage : une modification faite à la main sur un nœud est
rétablie au run suivant.

**Faire tourner la clé** : supprimer `/var/lib/flyc/deploy/id_ed25519*` sur l'hôte control, rejouer
le playbook. Une nouvelle paire est générée et la publique remplace l'ancienne sur tous les nœuds.

## L'inventaire vit en base

Les tables `nodes` et `platform_settings` de flyc-control sont la vérité. Le fichier YAML n'en est
qu'une projection : control le régénère à chaque tâche et le joint à la demande, le runner ne fait
qu'exécuter. `GET /v1/inventory` rend ce fichier tel qu'il sera joué — c'est la pièce d'audit à
regarder avant de lancer quoi que ce soit.

Conséquence la plus visible : ajouter un LB ne demande plus de redémarrer flyc-control. La liste
venait de `FLYC_EDGES`, une variable d'environnement lue une seule fois au démarrage ; elle est
maintenant relue en base à chaque passe de réconciliation.

**États d'un nœud** — `planned` (déclaré, pas encore installé), `installing`, `ready` (en service),
`draining` (en cours de retrait). Seuls les nœuds `ready` reçoivent de la configuration ; un nœud
`draining` reste dans l'inventaire — c'est par lui qu'on le démonte — mais on ne pousse plus vers
une machine qu'on est en train d'éteindre. Les tables `edges` et `queue_hosts` en dérivent :
inutile d'y toucher, elles sont reconstruites à chaque passe.

Un nœud `draining` reste dans l'inventaire — c'est par lui qu'on le démonte — mais les plays
d'installation l'excluent (`queue:!draining`, `edge:!draining`) : sans cela, un déploiement lancé
pendant un retrait le réinstallerait, jusqu'à le faire rejoindre le cluster Redis dont on venait de
le sortir.

**Amorçage** — au tout premier démarrage, control lit `/opt/flyc/inventory-seed.json`, rendu par
Ansible depuis l'inventaire d'installation, et remplit les tables. Ensuite il ne fait plus que
comparer : un écart entre le fichier et la base est signalé dans le journal (`l'inventaire fichier
diverge de la base, qui fait foi`) sans jamais être appliqué — sinon un nœud retiré depuis
l'interface réapparaîtrait au premier redéploiement lancé depuis un poste.

Deux pièges à connaître :

* ce JSON ne peut pas vivre dans `.env` : le parseur dotenv de docker compose ne survit pas à une
  valeur JSON et perd **toutes les variables suivantes**, `FLYC_MASTER_KEY` comprise (control
  refuse alors de démarrer) ;
* une base sans nœud n'est pas un ordre de tout effacer. Control garde sa topologie en mémoire et
  journalise `aucun nœud déclaré` ; c'est le symptôme d'un fichier d'amorçage absent ou illisible
  (il doit être lisible par le conteneur, qui tourne sous son propre uid : mode 0644).

**Déployer une nouvelle version** — le tag d'image est un réglage de plateforme, donc en base. Le
changer dans `inventory/*.yml` ne suffit plus : une tâche lancée depuis l'interface rejouerait la
version enregistrée et ferait donc un retour arrière silencieux. Le bon geste :

```sh
curl -X PUT -H "Authorization: Bearer <clé plateforme>" -H 'Content-Type: application/json' \
     -d '{"image_tag":"<sha>"}' http://127.0.0.1:8090/v1/settings
```

Les champs omis gardent leur valeur. Au démarrage, control compare le fichier d'amorçage à la base
et journalise les écarts (`image_tag : base "…", fichier "…"`) — c'est le signal qu'un déploiement
depuis un poste et l'interface ne parlent pas de la même version.

**Le mode fichier reste valable** : `ansible-playbook -i inventory/<env>.yml edge/site.yml` depuis
un poste continue de fonctionner, et `tools/add-node.sh` fait les deux — il écrit le fichier *et*
déclare le nœud dans control. Un nœud ajouté à la main dans le fichier, sans passer par l'API,
serait installé mais resterait inconnu de control : aucune configuration ne lui serait poussée.

**Réglages libres** — les variables qui ne sont pas des colonnes de `platform_settings` (par
exemple `haproxy_ticket_rate_limit`) vivent dans `extra_vars` et sont recopiées telles quelles dans
`all.vars`. Pour qu'un réglage ajouté au fichier d'inventaire soit repris à l'amorçage, il faut
l'ajouter à `extra_vars` dans `inventory-seed.json.j2` — ou le saisir dans l'interface.

**Certificat du registre** — `flyc_registry_ca` porte le certificat en contenu PEM, pas un chemin :
control et le runner n'ont pas accès au poste où le fichier avait été téléchargé. Le mode poste
accepte toujours `flyc_registry_ca_file`, et le contenu l'emporte quand les deux sont présents.

## Publier une version

Les images ne partent vers le registre public que sur un **tag git**. Un commit sur `main` ne
produit que des images internes : `latest`, côté public, désigne donc toujours une version publiée.

```sh
git tag v1.2.3 && git push origin v1.2.3
```

Le pipeline construit les trois images et les pousse sous `v1.2.3`, `v1.2` et `latest`. Il faut
`GHCR_USER` et `GHCR_TOKEN` (jeton GitHub, portée `write:packages`) dans les variables CI ; sans
eux la publication publique est simplement sautée, l'interne continue.

Attention à un détail de ghcr.io : un paquet créé par le premier push est **privé par défaut**. Le
rendre public une fois, dans les réglages du paquet sur GitHub, sinon les installations échouent
avec un `denied` peu parlant.

**Mettre à jour une plateforme** : la version déployée est un réglage en base, pas une valeur de
fichier. Plateforme → Infrastructure → Réglages, ou `PUT /v1/settings {"image_tag":"v1.2.4"}`, puis
« Déployer la plateforme ». Changer le tag dans un inventaire fichier ne suffit pas — une tâche
lancée depuis l'interface rejouerait la version enregistrée.

**Retour arrière** : remettre l'ancien tag et redéployer. Les migrations de base ne sont pas
réversibles : vérifier qu'aucune migration n'a été appliquée entre les deux versions avant de
descendre.

## Installer une plateforme depuis zéro

```sh
curl -fsSL https://github.com/flyc-io/flyc/-/raw/main/tools/install-control.sh -o install-control.sh
sudo sh install-control.sh --control-host control.exemple.fr --wait-host wait.exemple.fr \
     --image-tag <sha> --registry-ca /chemin/roots.pem
```

Le script pose Docker, crée le compte de déploiement et sa paire de clés, écrit un inventaire
d'installation d'un seul hôte, puis fait jouer `edge/site.yml` par l'image runner sur cette
machine. Il affiche à la fin l'URL de l'interface, le compte administrateur initial et la clé
plateforme. Il est rejouable : relancer la même commande après un échec reprend là où c'était.

La clé de déploiement est générée par le script et non par Ansible, parce qu'il faut l'avoir
autorisée avant de pouvoir se connecter à la machine. Le rôle `secrets` la retrouve ensuite et ne
la régénère pas.

## Enrôler un nœud

Interface → Plateforme → Infrastructure → « Ajouter un nœud » : choisir les rôles, puis coller sur
la machine la ligne affichée.

```sh
curl -fsSL https://control.exemple.fr/enroll/<jeton> | sudo sh
```

Le jeton est à usage unique et vaut quinze minutes. Le script crée le compte `flyc-deploy`, y
installe la **clé publique** de l'hôte control avec ses restrictions (`from=`, pas de redirection,
pas d'agent, pas de X11), écrit le fichier sudoers, puis annonce la machine à control avec son nom
et ses adresses. Rien de secret ne circule : le jeton ne sert qu'à lier la machine à une demande et
à empêcher un inconnu de se déclarer.

La machine apparaît alors dans l'interface. Lui attribuer ses rôles la déclare comme nœud
(`planned`) ; le bouton « Installer » enchaîne les tâches :

1. `edge/site.yml` sur toute la plateforme — installe le nœud et met à jour les autres (leurs règles
   de pare-feu énumèrent les adresses autorisées) ;
2. `tools/propagate-node.yml` — déclare le nœud sur les LB déjà en service, par la Dataplane API ;
3. mise en service (`ready`) et synchronisation immédiate ;
4. `tools/check-node.sh` — les neuf vérifications d'usage.

Un `--purge` efface les paquets, configurations et données Flyc, mais **laisse le compte
`flyc-deploy`** : c'est par lui que le playbook s'exécute, le retirer couperait la connexion en
cours. Le supprimer à la main (`userdel -r flyc-deploy`, `rm /etc/sudoers.d/flyc-deploy`) si la
machine quitte le parc.

Le retrait suit le chemin inverse depuis le même écran : mise en retrait, sortie du cluster Redis
s'il s'agit d'un hôte de file, retrait de la configuration des LB, mise hors service, sortie de
l'inventaire, convergence des nœuds restants. `tools/add-node.sh` fait exactement la même chose
depuis un poste, pour qui préfère la ligne de commande.

Les tâches s'exécutent sur l'hôte control, dans le runner : fermer l'onglet ne les interrompt pas,
et la page Tâches permet de rouvrir n'importe quel journal.

## L'exécuteur de tâches (flyc-runner)

Quand un déploiement est lancé depuis l'interface, ce n'est pas `flyc-control` qui joue Ansible :
il dépose une demande dans `/var/lib/flyc/jobs` et le conteneur `flyc-runner` l'exécute. Les deux
ne se parlent que par des fichiers (`<id>.request.json`, `.claim`, `.log`, `.result.json`), ce qui
permet à une tâche de survivre au redémarrage de control — utile puisqu'un déploiement redéploie
control lui-même.

Un point mérite d'être connu avant de s'en étonner : **le runner ne peut pas se remplacer
lui-même**. Recréer son conteneur tuerait le playbook en cours. Le rôle `flyc_compose` l'écarte
donc du `docker compose up` et confie son remplacement à un service systemd éphémère
(`flyc-runner-refresh-<horodatage>`), qui pose `/var/lib/flyc/jobs/.paused`, attend la fin de la
tâche en cours, recrée le conteneur, puis lève la pause.

Conséquences à l'exploitation :

* une tâche peut rester quelques secondes en `pending` juste après un déploiement : un
  remplacement du runner est en attente. `journalctl -u 'flyc-runner-refresh-*'` le montre ;
* si `.paused` traîne alors qu'aucun service de remplacement ne tourne, le runner l'ignore au bout
  de deux minutes (la date du fichier est rafraîchie tant que le script vit) ;
* `docker logs flyc-runner` donne le fil des tâches prises et terminées.

## Repères

| Élément | Où | Vérification rapide |
|---|---|---|
| LB (edge) | hôtes `edge`, HAProxy natif | `haproxy -c -f /etc/haproxy/haproxy.cfg`, `birdc show protocols`, `curl -sk https://<VIP wait>/healthz` |
| flyc-queue | hôtes `queue`, conteneur `flyc-queue` | `curl :8080/healthz` → redis et signing_key ok |
| Redis Cluster | hôtes `queue`, conteneurs `flyc-redis-a/b` | `redis-cli -p 7000 -a … cluster info` → `cluster_state:ok` |
| flyc-control | hôte `data`, conteneur `flyc-control` | `curl :8090/healthz`, `GET /v1/edges` → `in_sync: true` |
| Supervision | hôte `data`, Prometheus :9090, Alertmanager :9093, Grafana :3000 | `GET :9090/api/v1/targets` tout `up` |
| Secrets | hôte `data`, `/opt/flyc/secrets.env` | à sauvegarder : sans la clé maître, les clés de signature et certificats en base sont illisibles |

## Déployer une version

Sur une machine fraîchement installée, `unattended-upgrades` ou `apt.systemd.daily` peuvent tenir le
verrou apt pendant plusieurs minutes au moment du premier run : les tâches d'installation attendent
jusqu'à dix minutes (`lock_timeout`). Si un `apt-get update` reste bloqué bien plus longtemps (méthode
réseau en attente), le terminer est sans risque tant qu'aucun `dpkg` ne tourne :
`pgrep -fl dpkg` doit être vide, puis `kill` le `apt-get` et les processus `/usr/lib/apt/methods`.


```sh
ansible-playbook -i inventory/<env>.yml edge/site.yml -e flyc_image_tag=<sha ou tag>
```
Idempotent, rejouable. Ordre : data → queue → LB un par un avec vérification (config, BGP, VIP). Un LB en
échec arrête le play avant le suivant. **Retour arrière** : relancer avec le tag précédent.

Ne pas déployer pendant un événement : le reload HAProxy est transparent, mais le redémarrage des
replicas flyc-queue coupe les flux SSE (les navigateurs se reconnectent, la place est conservée).

## Activer / fermer une file le jour J

Interface → Files → boutons Ouvert / File / Fermé, ou `POST /v1/rooms/{slug}/state`. La réponse
arrive après la mise à jour des LB (maps à chaud, sans reload). `closed` renvoie tout le monde,
même les pass valides, vers la page de fermeture.

Vider la file (tickets en attente supprimés, pass émis conservés) : bouton Vider ou
`POST /v1/rooms/{slug}/flush`.

## Ajouter un client (tenant)

1. Plateforme → Tenants → Nouveau tenant : la clé API initiale s'affiche une seule fois.
2. Domaine (mode DNS ou iframe), backend (serveurs du client), file reliant les deux.
3. Le certificat arrive seul si `flyc_acme_email` est configuré ; suivre son statut dans Domaines.

## Un LB ne se synchronise plus (`in_sync: false`)

1. Lire `last_error` dans Plateforme → Infrastructure ou `GET /v1/edges`.
2. Cas fréquents : Dataplane API arrêtée (`systemctl status dataplaneapi` sur le LB), mot de passe
   différent après réinstallation (`secrets.env` vs `haproxy.cfg` userlist), reload HAProxy en
   échec (`journalctl -u haproxy`).
3. Relancer une passe : bouton « Synchroniser maintenant » ou `POST /v1/sync`.
4. Dernier recours : réappliquer le template et laisser le réconciliateur reconstruire :
   `ansible-playbook … --limit <lb> -e haproxy_force_config=true`.

## Ajouter un LB ou un hôte de file

```sh
tools/add-node.sh lb    lb2    203.0.113.12 2001:db8::12
tools/add-node.sh queue queue3 10.0.0.13
```

Le script écrit l'inventaire, rejoue le playbook sur toute la plateforme (nécessaire : les règles de
pare-feu des hôtes existants énumèrent les adresses autorisées), puis déclare le nœud sur les LB déjà
en service par la Dataplane API. `--check` décrit l'opération sans l'appliquer.

Le drop-in de routage de retour des VIP porte le nom de l'unité systemd-networkd qui configure
l'interface de sortie. Ce nom diffère d'une machine à l'autre : `10-netplan-eth0.network` sur une
image cloud-init, `eth0.network` sur une unité écrite à la main. Le rôle le demande à `networkctl`
plutôt que de le supposer ; `flyc_vip_network_unit` ne sert qu'à forcer un nom particulier. Un
drop-in posé sous un mauvais nom est ignoré en silence : aucune règle `ip rule`, table de routage
vide, et les réponses sourcées par les VIP se perdent.

Un hôte de file ajoute deux nœuds au cluster Redis : un maître, qui reçoit des slots par
rééquilibrage, et une réplique, placée sur un autre hôte que son maître. Le rééquilibrage déplace des
clés : opération en ligne, mais à faire hors événement. flyc-queue redémarre alors sur les hôtes
existants (leur liste de nœuds change) : les flux SSE se reconnectent et les visiteurs gardent leur
place.

Si un rééquilibrage est interrompu, des slots restent « ouverts » et bloquent les suivants. Le rôle
le détecte et applique `redis-cli --cluster fix` au run suivant ; à la main, la même commande depuis
un nœud du cluster.

## Retirer un nœud

```sh
tools/add-node.sh remove lb lb2            # nœud rendu inerte, tout reste installé
tools/add-node.sh --purge remove lb lb2    # machine remise à zéro
```

Le script refuse tant que les préalables ne sont pas faits : BIRD arrêté sur le LB
(`systemctl stop bird`, vérifier que le trafic a basculé), ou nœuds Redis sortis du cluster pour un
hôte de file (procédure ci-dessous).

Il enchaîne ensuite quatre gestes : mise hors service du nœud, retrait de l'inventaire, retrait de
sa déclaration sur les LB restants, convergence de la plateforme, puis suppression de sa ligne dans
flyc-control pour qu'il cesse d'apparaître dans l'interface.

**Sortir un nœud de l'inventaire ne suffit pas.** Ses services restent activés au démarrage : au
premier redémarrage, un LB réannonce les VIP en BGP et sert une configuration figée au moment du
retrait, avec des maps, des certificats et des backends périmés. Les visiteurs qui atterrissent
dessus voient des erreurs. C'est pourquoi le retrait met toujours le nœud hors service.

Deux niveaux, via `tools/decommission-node.yml` :

| Niveau | Ce qui est fait | Pour quoi |
|---|---|---|
| `inert`, par défaut | services arrêtés **et désactivés**, VIP retirées de la boucle locale, routage de retour supprimé ; paquets et configurations conservés | mise au repos réversible : `tools/add-node.sh` remet le nœud en service en un run |
| `purge`, avec `--purge` | en plus : paquets, dépôts, configurations, données et règles de pare-feu supprimés | la machine redevient une Ubuntu vierge |

Le playbook s'utilise aussi seul pour nettoyer une machine :
`ansible-playbook -i <inv> tools/decommission-node.yml -l <hôte> -e decommission_level=purge`.

### Ce qu'un retrait ne fait pas

Le script refuse de retirer un hôte qui porte plusieurs rôles Flyc, et le playbook de mise hors
service refuse également. L'assistant d'installation propose de placer `data` sur le premier hôte
de file, et le profil all-in-one cumule les trois : sur une telle machine, retirer « l'hôte de
file » arrêterait aussi flyc-control et Postgres, et `--purge` effacerait leurs données. Déplacer
les autres rôles d'abord.

`--purge` reste dans le périmètre de Flyc. Seuls les conteneurs du projet `flyc` sont supprimés ;
Docker n'est désinstallé, et `/var/lib/docker` effacé, que s'il ne portait rien d'autre. Le
pare-feu n'est jamais remis à zéro : seules les règles marquées `flyc` sont retirées, la politique
par défaut et les règles de l'administrateur restent en place. Une machine partagée n'est donc pas
cassée par le retrait d'un rôle Flyc.

Un retrait interrompu se reprend avec la même commande : l'identité du nœud et les étapes déjà
faites sont conservées dans `.flyc/` jusqu'à la fin, y compris après la modification de
l'inventaire. Rien n'y est écrit par un refus ni par `--check`.

### Ce que fait le retrait d'un hôte de file

Le script s'en charge, il n'y a rien à faire à la main. Dans l'ordre : il vide les slots du maître
(poids nul, redis-cli les répartit sur les maîtres restants), retire les deux nœuds du cluster,
replace les répliques, puis met la machine hors service.

Le vidage déplace de vraies clés : compter quelques minutes par shard, pendant lesquelles le
service continue. Le script refuse s'il ne resterait pas au moins trois hôtes de file, si le
cluster n'est pas sain avant l'opération, ou si la couverture des slots est incomplète après le
vidage — dans ce dernier cas il s'arrête sans rien retirer.

À la main, si jamais : `redis-topology.py detach <ip:port d'un nœud qui reste> <ip qui part>`,
déposé dans `/opt/flyc` sur les hôtes de file. Ne pas utiliser `redis-cli --cluster reshard` sans
arguments : il est interactif et, hors terminal, il boucle indéfiniment sur sa question.

## Déclarer un backend client

```sh
tools/add-node.sh backend <tenant> <nom> <ip:port> [ip:port…]
```

Équivalent de l'écran Backends de l'interface. Les serveurs appartiennent au client : Flyc ne les
installe pas, il les met en balance derrière la file.

## Une replica flyc-queue tombe

Rien à faire à chaud : les deux autres continuent, l'admitter d'une file tenue par la replica tombée
est repris en moins de 10 s (alerte `FlycAdmitterMissing` sinon). Les visiteurs connectés en SSE à
cette replica se reconnectent via HAProxy sur une autre, avec leur ticket. Redémarrer :
`docker restart flyc-queue` ou rejouer le playbook.

## Redis Cluster dégradé

`cluster_state:fail` : un shard sans maître. Vérifier les six nœuds (`docker ps`, `cluster nodes`).
Un hôte queue perdu = un maître et une replica d'un autre shard perdus ; le cluster survit à la
perte d'un seul hôte. Après remplacement de l'hôte, rejouer le playbook : le rôle `redis_cluster`
ne recrée pas un cluster existant, il faut réintégrer les nœuds à la main
(`redis-cli --cluster add-node`) puis rééquilibrer. Les tickets en attente sur un shard perdu sans
replica sont perdus : les visiteurs reprennent un ticket.

## Certificat en erreur

Domaines → statut `error` et `last_error`. Causes fréquentes : le domaine ne pointe pas encore sur la
VIP (HTTP-01 impossible), port 80 filtré en amont, limites Let's Encrypt. Corriger, puis « Réémettre ».
Le certificat de démarrage auto-signé sert en attendant.

## Rotation de la clé de signature

Plateforme → Infrastructure → « Faire tourner », ou `POST /v1/keys/rotate`. L'ancienne clé reste
acceptée 24 h. À faire hors événement (reload des LB). Si un LB était injoignable au moment de la
rotation, il reçoit les clés à sa prochaine synchronisation.

## Perte de l'hôte data (flyc-control)

Les LB et flyc-queue continuent avec leur dernière configuration ; l'administration, l'ACME et le
mode auto sont indisponibles. Restaurer : réinstaller l'hôte avec le même `secrets.env` et une
sauvegarde de Postgres (`/var/lib/flyc/postgres`). Sans la clé maître d'origine, les clés de
signature sont perdues : après réinstallation, faire une rotation et laisser expirer les pass.

## Sauvegardes à prévoir

* `/opt/flyc/secrets.env` (hôte data) — indispensable.
* Postgres : `docker exec flyc-postgres pg_dump -U flyc flyc > flyc-$(date +%F).sql` (tenants, domaines,
  files, certificats chiffrés, utilisateurs).
* Rien à sauvegarder sur les LB ni sur Redis : tout est reconstruit par le réconciliateur, les
  tickets en attente sont éphémères.

## Mode dégradé

Alerte `FlycDegradedMode` : un LB dépasse `haproxy_degrade_conn_threshold` connexions. Les nouveaux
visiteurs passent en polling long au lieu du SSE. Ajouter un LB (section ci-dessus) ou relever le seuil
si la machine a de la marge (RAM : ~40 Ko par connexion TLS côté HAProxy, ~65 Ko côté flyc-queue).
