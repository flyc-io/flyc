#!/usr/bin/env bash
# Ajoute un nœud à une installation Flyc en service : inventaire, installation du nœud, puis
# propagation aux LB déjà en place via la Dataplane API (aucune réécriture de leur haproxy.cfg,
# aucune coupure). Retire aussi un nœud proprement.
#
#   tools/add-node.sh lb      <nom> <ip4> [ip6]              ajoute un load balancer
#   tools/add-node.sh queue   <nom> <ip4>                    ajoute un hôte de file d'attente
#   tools/add-node.sh backend <tenant> <nom> <ip:port>…      déclare un backend client (API control)
#   tools/add-node.sh remove  lb|queue <nom>                 retire un nœud
#   tools/add-node.sh list                                   état de l'inventaire
#
# Options : -i <inventaire> (défaut $FLYC_INVENTORY ou inventory/test.yml)
#           -t <tag d'image>   --check (n'applique rien)   -y (sans confirmation)
#           --purge (avec remove : efface aussi paquets, configurations et données du nœud)
#           --full (rejoue tout sur tous les hôtes au lieu de la convergence ciblée)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INV="${FLYC_INVENTORY:-$ROOT/inventory/test.yml}"
TAG=""; CHECK=false; YES=false; PURGE=false; FULL=false

while [ $# -gt 0 ]; do
  case "$1" in
    -i) INV="$2"; shift 2;;
    -t) TAG="$2"; shift 2;;
    --check) CHECK=true; shift;;
    --purge) PURGE=true; shift;;
    --full) FULL=true; shift;;
    -y|--yes) YES=true; shift;;
    -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    --) shift; break;;
    -*) echo "option inconnue : $1" >&2; exit 2;;
    *) break;;
  esac
done

# shellcheck source=/dev/null
[ -f "$ROOT/tools/ansible-env.sh" ] && . "$ROOT/tools/ansible-env.sh"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
die()  { printf '\033[31m%s\033[0m\n' "$*" >&2; exit 1; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

[ -f "$INV" ] || die "inventaire introuvable : $INV"
INVEDIT="$ROOT/tools/inventory-edit.py"

confirm() { # confirm "question"
  $YES && return 0
  read -r -p "$1 [o/N] : " a
  case "$(printf %s "$a" | tr '[:upper:]' '[:lower:]')" in o*|y*) return 0;; *) die "abandon";; esac
}

inv_hosts() { "$INVEDIT" list "$INV" "$1" | cut -d' ' -f2-; }
inv_ip()    { ansible-inventory -i "$INV" --host "$1" 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("mgmt_ip",""))'; }
data_host() { inv_hosts data | awk '{print $1}'; }

# Roles Flyc portes par un hote. Une machine peut en porter plusieurs : l'assistant propose de
# mettre data sur le premier hote de file, et le profil all-in-one les cumule tous.
node_roles() {
  local n="$1" out="" g
  for g in edge queue data; do
    if inv_hosts "$g" | tr ' ' '\n' | grep -qx "$n"; then out="$out $g"; fi
  done
  echo "${out# }"
}

# --check ne doit rien laisser derriere lui, mais doit simuler fidelement : on travaille sur une
# copie jetable de l'inventaire, qu'on modifie vraiment, et Ansible tourne dessus en mode check.
scratch_inventory() {
  local d; d=$(mktemp -d "${TMPDIR:-/tmp}/flyc-check.XXXXXX")
  cp "$INV" "$d/inventory.yml"
  SCRATCH_DIR="$d"; INV="$d/inventory.yml"
  echo "  (--check) inventaire simule dans $INV, l'original n'est pas touche"
}
SCRATCH_DIR=""
# Le statut du trap EXIT devient celui du script quand il se termine sans exit explicite : une
# derniere commande fausse (ici le test quand il n'y a rien a nettoyer) ferait sortir en 1 chaque
# execution reussie.
cleanup() {
  if [ -n "$SCRATCH_DIR" ]; then rm -rf "$SCRATCH_DIR"; fi
  return 0
}
trap cleanup EXIT

# Etat de progression d'un retrait : les etapes qui suivent la modification de l'inventaire ne
# peuvent plus retrouver le noeud par l'inventaire. On garde son identite jusqu'a la fin, ce qui
# rend la commande reprenable telle quelle apres un echec.
STATE=""
# Le chemin est calcule sur l'inventaire d'origine, avant toute bascule sur une copie jetable.
st_path() { STATE="$ROOT/.flyc/remove-$(basename "$INV" .yml)-$1.state"; }
# Le fichier n'est cree qu'au moment d'agir : un retrait refuse ou simule ne doit rien laisser.
st_begin() { # st_begin <kind> <group> <ip>
  $CHECK && return 0
  mkdir -p "$ROOT/.flyc"
  [ -f "$STATE" ] || printf 'kind=%s\ngroup=%s\nip=%s\n' "$1" "$2" "$3" > "$STATE"
}
st_get()  { sed -n "s/^$1=//p" "$STATE" 2>/dev/null | head -1; }
st_done() { grep -qx "done:$1" "$STATE" 2>/dev/null; }
st_mark() { echo "done:$1" >> "$STATE"; }

# Appel de l'API de flyc-control depuis l'hôte data. Le corps passe par un bloc JSON unique :
# « -e cle=valeur » serait découpé sur les espaces par Ansible.
control_api() {
  local method="$1" path="$2" body="${3:-}" vars
  vars=$(python3 -c 'import json, sys
v = {"api_method": sys.argv[1], "api_path": sys.argv[2]}
if len(sys.argv) > 3 and sys.argv[3]:
    v["api_body"] = json.loads(sys.argv[3])
print(json.dumps(v))' "$method" "$path" "$body") || die "parametres d API invalides"
  ansible-playbook -i "$INV" "$ROOT/tools/control-api.yml" -e "$vars"
}

# Déclaration d'un noeud dans la base de flyc-control, qui fait foi pour l'inventaire. Sans elle le
# noeud est installé mais reste inconnu de control : aucune config ne lui est poussée.
declare_node() {
  local name="$1" role="$2" ip="$3" ip6="${4:-}"
  control_api POST /v1/nodes "$(python3 -c 'import json, sys
n = {"name": sys.argv[1], "roles": [sys.argv[2]], "mgmt_ip": sys.argv[3], "state": "ready"}
if len(sys.argv) > 4 and sys.argv[4]:
    n["mgmt_ip6"] = sys.argv[4]
print(json.dumps(n))' "$name" "$role" "$ip" "$ip6")"
}

st_clear(){ rm -f "$STATE"; }

is_ip4() { [[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; }

check_ssh() { # check_ssh <ip>
  local u; u=$(ansible-inventory -i "$INV" --list 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("all",{}).get("vars",{}).get("ansible_user","ubuntu"))')
  ssh -n -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new "$u@$1" 'sudo -n true' >/dev/null 2>&1 \
    || die "ssh ou sudo sans mot de passe impossible sur $1 (corriger avant d'ajouter le nœud)"
  echo "  ssh + sudo ok sur $1"
}

# Secrets lus sur l'hôte data (mot de passe Dataplane API, utilisés pour la propagation)
secrets_cache=""
secrets() { # secrets CLÉ
  if [ -z "$secrets_cache" ]; then
    secrets_cache=$(ansible -i "$INV" "$(data_host)" -b -m ansible.builtin.slurp -a "src=/opt/flyc/secrets.env" 2>/dev/null \
      | sed -n 's/.*"content": "\([^"]*\)".*/\1/p' | base64 -d 2>/dev/null) \
      || die "impossible de lire /opt/flyc/secrets.env sur $(data_host)"
    [ -n "$secrets_cache" ] || die "secrets vides sur $(data_host) : l'installation de base est-elle faite ?"
  fi
  printf '%s\n' "$secrets_cache" | sed -n "s/^$1=//p"
}

# L'arrivee ou le depart d'un noeud change aussi l'etat attendu des AUTRES hotes : leurs regles de
# pare-feu enumerent les adresses autorisees, et les hotes de file portent la liste des noeuds
# (REDIS_ADDRS, FLYC_QUEUE_HOSTS). Sans cela le nouveau noeud est installe mais reste filtre.
# Rejouer tout le playbook partout suffirait, mais coute plusieurs minutes pour deux tâches utiles :
# on ne leur applique donc que ce qui les concerne. --full retablit la convergence complete.
ansible_run() {
  bold "ansible-playbook $*"
  ansible-playbook -i "$INV" "$ROOT/edge/site.yml" "$@" \
    ${TAG:+-e "flyc_image_tag=$TAG"} $($CHECK && echo "--check --diff" || true)
}

# converge_others <lb|queue> [noeud a exclure]
# L'hote data n'est jamais exclu : c'est lui qui porte les secrets que les autres relisent.
converge_others() {
  if $FULL; then step "Convergence complete de la plateforme"; ansible_run; return; fi
  local kind="$1" excl="${2:-}" tags=firewall
  [ "$kind" = queue ] && tags=firewall,compose
  step "Convergence ciblee des autres hotes (--tags $tags)"
  if [ -n "$excl" ]; then ansible_run --limit "all:!$excl" --tags "$tags"; else ansible_run --tags "$tags"; fi
}

install_node() {      # install_node <noeud>
  if $CHECK; then
    step "Installation de $1 : non simulable"
    echo "  la machine est encore nue, --check ne peut pas simuler une installation :"
    echo "  les taches qui dependent des paquets absents echoueraient sans que cela veuille dire"
    echo "  quoi que ce soit. La simulation ci-dessus porte sur les hotes deja en service."
    return 0
  fi
  step "Installation de $1 et mise a jour de flyc-control"
  if $FULL; then ansible_run; else ansible_run --limit "$1,$(data_host)"; fi
}

# ---- propagation aux LB déjà en service (Dataplane API, depuis l'hôte data) ---------------------
propagate() { # propagate queue|lb <nom> <ip>
  local kind="$1" name="$2" ip="$3"
  local targets; targets=$(inv_hosts edge | tr ' ' '\n' | grep -vx "$name" | paste -sd, -)
  [ -n "$targets" ] || { echo "  aucun LB déjà en service, rien à propager"; return 0; }
  step "Propagation vers les LB en service : $targets"
  ansible-playbook -i "$INV" "$ROOT/tools/propagate-node.yml" \
    -e "propagate_kind=$kind" -e "propagate_name=$name" -e "propagate_ip=$ip" \
    -e "propagate_targets=$targets" $($CHECK && echo --check || true)
}

cmd="${1:-}"; shift || true
case "$cmd" in

list)
  bold "Inventaire : $INV"
  "$INVEDIT" list "$INV"
  ;;

lb)
  name="${1:-}"; ip="${2:-}"; ip6="${3:-}"
  { [ -n "$name" ] && [ -n "$ip" ]; } || die "usage : tools/add-node.sh lb <nom> <ip4> [ip6]"
  is_ip4 "$ip" || die "IPv4 invalide : $ip"
  # Déjà dans l'inventaire : reprise d'une opération interrompue, tant que l'adresse concorde.
  resume=false
  if inv_hosts edge | tr ' ' '\n' | grep -qx "$name"; then
    cur=$(inv_ip "$name")
    [ "$cur" = "$ip" ] || die "$name est déjà dans le groupe edge avec l'adresse $cur (demandée : $ip)"
    resume=true
  fi
  step "Vérifications"
  check_ssh "$ip"
  echo "  LB en service : $(inv_hosts edge)"
  echo "  le nouveau LB annoncera les mêmes VIP en BGP et recevra la config depuis flyc-control"
  echo "  les LB existants sont repris un par un (pare-feu, section peers) sans interruption"
  if $resume; then echo "  $name est déjà dans l'inventaire : reprise de l'installation"; fi
  $CHECK || confirm "$($resume && echo Reprendre || echo Ajouter) le LB $name ($ip) et converger la plateforme ?"
  step "Inventaire"
  attrs=(ansible_host="$ip" mgmt_ip="$ip")
  if [ -n "$ip6" ]; then attrs+=(mgmt_ip6="$ip6"); fi
  if $CHECK; then scratch_inventory; fi
  "$INVEDIT" add "$INV" edge "$name" "${attrs[@]}" || [ $? -eq 3 ]
  converge_others lb "$name"
  install_node "$name"
  propagate lb "$name" "$ip"
  # Le reconciliateur repasse toutes les 60 s ; on force la poussee pour que le LB serve tout de suite.
  # Cet appel modifie l'etat des LB : jamais en simulation.
  if $CHECK; then
    echo
    echo "  (--check) termine : ni synchronisation ni verification, rien n'a ete modifie"
  else
    step "Declaration dans flyc-control"
    declare_node "$name" edge "$ip" "$ip6"
    step "Synchronisation immediate depuis flyc-control"
    control_api POST /v1/sync
    step "Verification"
    "$ROOT/tools/check-node.sh" -i "$INV" lb "$name" \
      || die "le LB $name n'est pas operationnel : voir les contrôles en echec ci-dessus.
  L'inventaire le contient deja ; corriger puis relancer la meme commande."
  fi
  ;;

queue)
  name="${1:-}"; ip="${2:-}"
  { [ -n "$name" ] && [ -n "$ip" ]; } || die "usage : tools/add-node.sh queue <nom> <ip4>"
  is_ip4 "$ip" || die "IPv4 invalide : $ip"
  # Déjà dans l'inventaire : reprise d'une opération interrompue, tant que l'adresse concorde.
  resume=false
  if inv_hosts queue | tr ' ' '\n' | grep -qx "$name"; then
    cur=$(inv_ip "$name")
    [ "$cur" = "$ip" ] || die "$name est déjà dans le groupe queue avec l'adresse $cur (demandée : $ip)"
    resume=true
  fi
  step "Vérifications"
  check_ssh "$ip"
  echo "  hôtes queue en service : $(inv_hosts queue)"
  echo "  ses deux nœuds Redis rejoindront le cluster existant (un maître, une réplique d'un autre hôte)"
  echo "  les slots seront rééquilibrés : opération en ligne, mais à faire hors événement"
  echo "  flyc-queue redémarrera sur les hôtes existants (nouvelle liste de nœuds) : les flux SSE"
  echo "  se reconnectent, les visiteurs gardent leur place"
  if $resume; then echo "  $name est déjà dans l'inventaire : reprise de l'installation"; fi
  $CHECK || confirm "$($resume && echo Reprendre || echo Ajouter) l'hôte queue $name ($ip) et converger la plateforme ?"
  step "Inventaire"
  if $CHECK; then scratch_inventory; fi
  "$INVEDIT" add "$INV" queue "$name" ansible_host="$ip" mgmt_ip="$ip" || [ $? -eq 3 ]
  converge_others queue "$name"
  install_node "$name"
  propagate queue "$name" "$ip"
  if $CHECK; then
    echo
    echo "  (--check) termine : ni synchronisation ni verification, rien n'a ete modifie"
  else
    step "Declaration dans flyc-control"
    declare_node "$name" queue "$ip"
    step "Synchronisation immediate depuis flyc-control"
    control_api POST /v1/sync
    step "Verification"
    "$ROOT/tools/check-node.sh" -i "$INV" queue "$name" \
      || die "l'hote de file $name n'est pas operationnel : voir les contrôles en echec ci-dessus.
  L'inventaire le contient deja ; corriger puis relancer la meme commande."
  fi
  ;;

backend)
  tenant="${1:-}"; name="${2:-}"; shift 2 || true
  { [ -n "$tenant" ] && [ -n "$name" ]; } || die "usage : tools/add-node.sh backend <tenant> <nom> <ip:port>..."
  [ $# -gt 0 ] || die "au moins un serveur attendu, au format ip:port"
  step "Declaration du backend $name (tenant $tenant)"
  echo "  serveurs : $*"
  echo "  les serveurs appartiennent au client : Flyc ne les installe pas, il les met en balance"
  if $CHECK; then echo "  (--check) aucun appel effectue"; exit 0; fi
  confirm "Creer ou remplacer le backend $name du tenant $tenant ?"
  # Un seul bloc JSON : Ansible decouperait un -e cle=valeur contenant des espaces.
  vars=$(python3 "$ROOT/tools/backend-vars.py" "$tenant" "$name" "$@") || die "parametres invalides, rien n'a ete envoye"
  ansible-playbook -i "$INV" "$ROOT/tools/control-api.yml" -e "$vars"
  ;;

remove)
  kind="${1:-}"; name="${2:-}"
  { [ -n "$kind" ] && [ -n "$name" ]; } || die "usage : tools/add-node.sh [--purge] remove lb|queue <nom>"
  case "$kind" in lb) group=edge;; queue) group=queue;; *) die "type inconnu : $kind (lb ou queue)";; esac
  st_path "$name"
  if $CHECK; then scratch_inventory; fi

  if inv_hosts "$group" | tr ' ' '\n' | grep -qx "$name"; then
    ip=$(inv_ip "$name")
    # Une machine peut porter plusieurs roles : retirer l'un d'eux arreterait aussi les autres,
    # jusqu'a effacer Postgres et flyc-control si elle porte data. On refuse tant qu'ils n'ont
    # pas ete deplaces ailleurs.
    roles=$(node_roles "$name")
    [ "$roles" = "$group" ] || die "$name porte aussi le role : ${roles/$group/} (roles : $roles).
  Retirer ce noeud arreterait ces fonctions et, avec --purge, effacerait leurs donnees.
  Deplacer d'abord les autres roles sur une autre machine, puis relancer."
    [ "$(inv_hosts "$group" | wc -w)" -gt 1 ] || die "$name est le dernier hote du groupe $group : refus"
  elif [ -f "$STATE" ] && grep -q '^done:' "$STATE"; then
    # Retrait interrompu apres la modification de l'inventaire : on reprend ou on s'etait arrete.
    ip=$(st_get ip); kind=$(st_get kind); group=$(st_get group)
    echo "  retrait de $name deja engage, reprise (etapes faites : $(grep -c '^done:' "$STATE"))"
  else
    die "$name absent du groupe $group"
  fi

  level=$($PURGE && echo purge || echo inert)
  step "Retrait de $name ($ip) du groupe $group"

  if [ "$kind" = queue ]; then
    remaining=$(inv_hosts queue | tr ' ' '\n' | grep -vx "$name" | wc -w | tr -d ' ')
    [ "$remaining" -ge 3 ] || die "il ne resterait que $remaining hotes de file : le cluster Redis en veut au moins 3."
    echo "  ses noeuds Redis seront sortis du cluster s'ils y sont encore"
    echo "  le vidage de leurs slots deplace de vraies cles : comptez quelques minutes par shard,"
    echo "  le service continue pendant l'operation"
  else
    # Diagnostic bloquant : une lecture impossible ne doit pas passer pour un BGP arrete.
    bird=$(ansible -i "$INV" "$name" -b -m ansible.builtin.shell -a "systemctl is-active bird || true" 2>/dev/null | tail -n +2 | tr -d '[:space:]') \
      || die "impossible d'interroger $name : verifier l'acces avant de retirer le noeud"
    [ -n "$bird" ] || die "etat de BIRD sur $name illisible : ne pas retirer un LB qui annonce peut-etre encore les VIP"
    [ "$bird" != active ] || die "BIRD est encore actif sur $name : il annonce toujours les VIP.
  L'arreter (systemctl stop bird sur $name), verifier que le trafic a bascule, puis relancer."
    echo "  BIRD n'annonce plus les VIP depuis $name ($bird)"
  fi

  if [ "$level" = purge ]; then
    echo "  --purge : paquets, configurations et donnees Flyc de $name seront effaces"
  else
    echo "  les services de $name seront arretes ET desactives, ses VIP retirees de la boucle locale"
    echo "  tout reste installe : --purge pour effacer, ou add-node.sh pour le remettre en service"
  fi
  if $CHECK; then
    step "Simulation"
    ansible-playbook -i "$INV" "$ROOT/tools/decommission-node.yml" -l "$name" \
      -e "decommission_level=$level" -e "decommission_role=$kind" --check --diff || true
    echo
    echo "  (--check) termine, rien n'a ete modifie"
    exit 0
  fi
  confirm "Retirer $name de la plateforme ($level) ?"
  st_begin "$kind" "$group" "$ip"

  # Mise en retrait d'abord : control cesse de lui pousser de la configuration avant qu'on ne
  # commence a l'eteindre, sinon chaque passe echouerait bruyamment sur un service deja arrete.
  if ! st_done drain; then
    step "Mise en retrait dans flyc-control"
    control_api PATCH "/v1/nodes/$name" '{"state":"draining"}'
    st_mark drain
  fi

  # Les slots doivent partir avant que la machine ne s'arrete. Le playbook sait dire « rien a
  # faire » et refuse un cluster malsain : on le joue toujours plutot que de deviner ici.
  if [ "$kind" = queue ] && ! st_done redis; then
    step "Sortie des noeuds Redis du cluster"
    ansible-playbook -i "$INV" "$ROOT/tools/redis-detach.yml" -e "detach_node=$name"
    st_mark redis
  fi

  if ! st_done decommission; then
    step "Mise hors service de $name ($level)"
    ansible-playbook -i "$INV" "$ROOT/tools/decommission-node.yml" -l "$name" \
      -e "decommission_level=$level" -e "decommission_role=$kind"
    st_mark decommission
  fi

  if ! st_done inventory; then
    step "Inventaire"
    "$INVEDIT" remove "$INV" "$group" "$name" || [ $? -eq 3 ]
    st_mark inventory
  fi

  if ! st_done propagate; then
    step "Retrait de la configuration des LB en service"
    ansible-playbook -i "$INV" "$ROOT/tools/propagate-node.yml" \
      -e "propagate_kind=$kind" -e "propagate_name=$name" -e "propagate_ip=$ip" \
      -e "propagate_state=absent" -e "propagate_targets=$(inv_hosts edge | tr ' ' ',')"
    st_mark propagate
  fi

  if ! st_done control; then
    step "Mise a jour de flyc-control"
    ansible_run --limit "$(data_host)"
    st_mark control
  fi

  if ! st_done converge; then
    converge_others "$kind"
    st_mark converge
  fi

  # Le noeud sort de l'inventaire en base : les lignes edges et queue_hosts, qui en derivent, sont
  # supprimees par control a la passe suivante.
  if ! st_done dbdelete; then
    step "Retrait de $name dans flyc-control"
    control_api DELETE "/v1/nodes/$name"
    st_mark dbdelete
  fi
  st_clear
  ;;

*) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2;;
esac
