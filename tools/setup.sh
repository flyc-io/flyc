#!/usr/bin/env bash
# Assistant d'inventaire Flyc : pose les questions, vérifie l'accès SSH, écrit inventory/<env>.yml.
# Usage : tools/setup.sh [nom-env]      (défaut : mine)
set -euo pipefail

ENV_NAME="${1:-mine}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/inventory/$ENV_NAME.yml"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
ask() { # ask "question" "défaut" -> réponse
  local q="$1" def="${2:-}" ans
  if [ -n "$def" ]; then read -r -p "$q [$def] : " ans; echo "${ans:-$def}"; else
    while true; do read -r -p "$q : " ans; [ -n "$ans" ] && { echo "$ans"; return; }; echo "  (obligatoire)" >&2; done; fi
}
ask_yn() { local ans; read -r -p "$1 [o/N] : " ans; case "$(printf "%s" "$ans" | tr "[:upper:]" "[:lower:]")" in o*|y*) return 0;; *) return 1;; esac; }
is_ip4() { [[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; }
is_ip6() { [[ "$1" == *:* ]]; }
check_ssh() { # user host
  if ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$1@$2" 'sudo -n true' >/dev/null 2>&1; then echo "    ssh + sudo ok"; else
    echo "    ⚠ ssh ou sudo sans mot de passe impossible sur $2 (on continue, à corriger avant le run)"; fi
}

if [ -e "$OUT" ]; then
  ask_yn "$OUT existe déjà, l'écraser ?" || { echo "abandon"; exit 1; }
fi

bold "Flyc — assistant d'inventaire ($ENV_NAME)"
echo "Réponds aux questions ; les valeurs entre crochets sont les défauts. Aucun secret n'est demandé."
echo

SSH_USER=$(ask "Utilisateur SSH (avec sudo sans mot de passe)" "ubuntu")

bold "Load balancers (hôtes edge)"
ALL_IN_ONE=false
if ask_yn "Installation sur une seule machine (LB + file + control, sans BGP) ?"; then
  ALL_IN_ONE=true
  BOX_IP=$(ask "IP publique de la machine")
  check_ssh "$SSH_USER" "$BOX_IP"
  EDGE_NAMES=(box); EDGE_IPS=("$BOX_IP"); EDGE_IP6S=("")
  QUEUE_NAMES=(box); QUEUE_IPS=("$BOX_IP")
  VIP_APP4="$BOX_IP"; VIP_APP6=""; VIP_WAIT4="$BOX_IP"; VIP_WAIT6=""
  BGP_ENABLED=false
else
  N_EDGE=$(ask "Nombre de load balancers" "2")
  EDGE_NAMES=(); EDGE_IPS=(); EDGE_IP6S=()
  for ((i=0;i<N_EDGE;i++)); do
    name=$(ask "  Nom du LB $((i+1))" "lb$i")
    ip=$(ask "  IP de management IPv4 de $name (SSH + source BGP)")
    ip6=$(ask "  IPv6 de management de $name (vide : détection sur l'hôte)" " "); ip6="${ip6// /}"
    check_ssh "$SSH_USER" "$ip"
    if [ -z "$ip6" ]; then
      ip6=$(ssh -o BatchMode=yes -o ConnectTimeout=6 "$SSH_USER@$ip" "ip -6 -o addr show scope global 2>/dev/null | awk '{print \$4}' | cut -d/ -f1 | head -1" 2>/dev/null || true)
      if [ -n "$ip6" ]; then echo "    IPv6 détectée : $ip6 (source des sessions BGP v6)"; fi
    fi
    EDGE_NAMES+=("$name"); EDGE_IPS+=("$ip"); EDGE_IP6S+=("$ip6")
  done
  echo
  bold "VIP (adresses annoncées par tous les LB)"
  VIP_APP4=$(ask "VIP IPv4 des domaines protégés et de l'UI")
  VIP_APP6=$(ask "VIP IPv6 correspondante (vide si aucune)" " "); VIP_APP6="${VIP_APP6// /}"
  if ask_yn "VIP distincte pour la salle d'attente (recommandé en production) ?"; then
    VIP_WAIT4=$(ask "VIP IPv4 de la salle d'attente")
    VIP_WAIT6=$(ask "VIP IPv6 de la salle d'attente (vide si aucune)" " "); VIP_WAIT6="${VIP_WAIT6// /}"
  else VIP_WAIT4="$VIP_APP4"; VIP_WAIT6="$VIP_APP6"; fi
  echo
  bold "BGP"
  if ask_yn "Annoncer les VIP en BGP (sinon elles doivent être routées vers les LB autrement) ?"; then
    BGP_ENABLED=true
    LOCAL_AS=$(ask "AS local des LB" "64512")
    NEIGH=()
    while true; do
      nip=$(ask "  IPv4 d'un voisin BGP (vide pour terminer)" " "); nip="${nip// /}"
      [ -z "$nip" ] && break
      nas=$(ask "  AS du voisin $nip")
      NEIGH+=("$nip|$nas|v4")
    done
    if [ -n "$VIP_APP6$VIP_WAIT6" ]; then
      echo "  Les VIP IPv6 doivent être annoncées par une session BGP IPv6 (source : IPv6 de management des LB)."
      while true; do
        nip=$(ask "  IPv6 d'un voisin BGP (vide pour terminer)" " "); nip="${nip// /}"
        [ -z "$nip" ] && break
        def_as=""; [ ${#NEIGH[@]} -gt 0 ] && def_as=$(printf '%s' "${NEIGH[0]}" | cut -d'|' -f2)
        nas=$(ask "  AS du voisin $nip" "$def_as")
        NEIGH+=("$nip|$nas|v6")
      done
    fi
    [ ${#NEIGH[@]} -eq 0 ] && { echo "  aucun voisin : BGP désactivé"; BGP_ENABLED=false; }
  else BGP_ENABLED=false; fi
  echo
  bold "Routage de retour des VIP"
  echo "  Si la route par défaut des LB n'est pas l'équipement BGP (ex. un firewall qui fait du NAT),"
  echo "  les réponses sourcées par les VIP doivent repartir vers l'équipement BGP."
  VIP_GW4=$(ask "  Passerelle IPv4 de retour pour les VIP (vide si la route par défaut convient)" " "); VIP_GW4="${VIP_GW4// /}"
  VIP_GW6=$(ask "  Passerelle IPv6 de retour pour les VIP (vide si la route par défaut convient)" " "); VIP_GW6="${VIP_GW6// /}"
  VIP_UNIT="eth0.network"
  [ -n "$VIP_GW4$VIP_GW6" ] && VIP_UNIT=$(ask "  Unité systemd-networkd de l'interface de sortie" "eth0.network")
  echo
  bold "Hôtes queue (file d'attente + Redis Cluster, minimum 3)"
  N_QUEUE=$(ask "Nombre d'hôtes queue" "3")
  QUEUE_NAMES=(); QUEUE_IPS=()
  for ((i=0;i<N_QUEUE;i++)); do
    name=$(ask "  Nom de l'hôte queue $((i+1))" "queue$i")
    ip=$(ask "  IP de $name (joignable par les LB)")
    QUEUE_NAMES+=("$name"); QUEUE_IPS+=("$ip")
    check_ssh "$SSH_USER" "$ip"
  done
  echo
  bold "Hôte control (interface d'administration + Postgres)"
  CTL_IP=""
  if ask_yn "Machine dédiée pour control (recommandé) ? Sinon il tourne sur ${QUEUE_NAMES[0]}"; then
    CTL_NAME=$(ask "  Nom de l'hôte control" "control0")
    CTL_IP=$(ask "  IP de $CTL_NAME (joignable par les LB et les hôtes queue)")
    check_ssh "$SSH_USER" "$CTL_IP"
  fi
fi

echo
bold "Domaines de la plateforme"
WAIT_HOST=$(ask "Nom de la salle d'attente (pointe sur la VIP wait)" "wait.example.com")
CONTROL_HOST=$(ask "Nom de l'interface d'administration (pointe sur la VIP app)" "control.example.com")
echo
bold "Images"
IMG_QUEUE=$(ask "Image flyc-queue (le tag est remplacé par flyc_image_tag)" "registry.example.com/flyc/flyc-queue:{{ flyc_image_tag }}")
IMG_CONTROL=$(ask "Image flyc-control" "registry.example.com/flyc/flyc-control:{{ flyc_image_tag }}")
ACME_EMAIL=$(ask "Email pour les certificats Let's Encrypt (vide : pas d'ACME, certificat auto-signé)" " "); ACME_EMAIL="${ACME_EMAIL// /}"
REG_CA=""
if ask_yn "Le registre est signé par une CA privée (fichier racine à installer sur les hôtes Docker) ?"; then
  REG_CA=$(ask "  Chemin local du certificat racine (PEM)")
fi
echo
FW=true; ask_yn "Un firewall amont protège déjà ces machines (alors ufw ne sera pas géré) ?" && FW=false

{
  echo "# Généré par tools/setup.sh le $(date -u +%Y-%m-%dT%H:%MZ). Modifiable à la main."
  echo "all:"
  echo "  vars:"
  echo "    flyc_env: $ENV_NAME"
  echo "    ansible_user: $SSH_USER"
  echo "    ansible_become: true"
  echo "    flyc_wait_host: $WAIT_HOST"
  echo "    flyc_control_host: $CONTROL_HOST"
  echo "    flyc_vip_app:  { v4: \"$VIP_APP4\", v6: \"$VIP_APP6\" }"
  echo "    flyc_vip_wait: { v4: \"$VIP_WAIT4\", v6: \"$VIP_WAIT6\" }"
  if $BGP_ENABLED; then
    echo "    flyc_bgp:"
    echo "      enabled: true"
    echo "      local_as: $LOCAL_AS"
    echo "      neighbors:"
    for n in "${NEIGH[@]}"; do IFS='|' read -r nip nas fam <<<"$n"; echo "        - { ip: \"$nip\", remote_as: $nas, family: $fam }"; done
  else
    echo "    flyc_bgp: { enabled: false }"
  fi
  if [ -n "${VIP_GW4:-}${VIP_GW6:-}" ]; then
    echo "    flyc_vip_gateway: { v4: \"${VIP_GW4:-}\", v6: \"${VIP_GW6:-}\" }"
    echo "    flyc_vip_network_unit: $VIP_UNIT"
  fi
  echo "    flyc_image_queue: \"$IMG_QUEUE\""
  echo "    flyc_image_control: \"$IMG_CONTROL\""
  echo "    flyc_manage_firewall: $FW"
  # « [ … ] && echo » renvoie 1 quand le test est faux : sous set -e, l'assistant s'arrêterait en
  # plein milieu de l'inventaire dès qu'un de ces champs est laissé vide.
  if [ -n "$ACME_EMAIL" ]; then echo "    flyc_acme_email: $ACME_EMAIL"; fi
  if [ -n "$REG_CA" ]; then echo "    flyc_registry_ca_file: \"$REG_CA\""; fi
  echo "  children:"
  echo "    edge:"
  echo "      hosts:"
  for i in "${!EDGE_NAMES[@]}"; do
    echo "        ${EDGE_NAMES[$i]}: { ansible_host: \"${EDGE_IPS[$i]}\", mgmt_ip: \"${EDGE_IPS[$i]}\", mgmt_ip6: \"${EDGE_IP6S[$i]}\" }"
  done
  echo "    queue:"
  echo "      hosts:"
  for i in "${!QUEUE_NAMES[@]}"; do
    if $ALL_IN_ONE; then echo "        ${QUEUE_NAMES[$i]}:"; else
    echo "        ${QUEUE_NAMES[$i]}: { ansible_host: \"${QUEUE_IPS[$i]}\", mgmt_ip: \"${QUEUE_IPS[$i]}\" }"; fi
  done
  echo "    # Nœuds en cours de retrait : exclus des plays d'installation par edge/site.yml."
  echo "    draining:"
  echo "      hosts: {}"
  echo "    data:"
  echo "      hosts:"
  if $ALL_IN_ONE || [ -z "${CTL_IP:-}" ]; then echo "        ${QUEUE_NAMES[0]}:"; else
  echo "        $CTL_NAME: { ansible_host: \"$CTL_IP\", mgmt_ip: \"$CTL_IP\" }"; fi
} > "$OUT"

echo
bold "Inventaire écrit : $OUT"
echo "Prochaine étape :"
echo "  ansible-galaxy collection install -r edge/requirements.yml"
echo "  ansible-playbook -i inventory/$ENV_NAME.yml edge/site.yml"
