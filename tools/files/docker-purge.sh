#!/usr/bin/env bash
# Retire les ressources Docker de Flyc sur une machine mise hors service, sans toucher à ce qui
# ne lui appartient pas.
#
# La machine peut être partagée : effacer /var/lib/docker ou arrêter le démon y détruirait des
# services sans rapport. On supprime donc d'abord les seuls conteneurs et volumes du projet Flyc,
# puis on regarde ce qu'il reste. Docker n'est arrêté et ses données effacées que s'il ne sert
# plus à rien d'autre.
#
# Sortie : « docker_dedie=oui » ou « docker_dedie=non », que le playbook lit pour décider s'il
# peut désinstaller Docker et supprimer /var/lib/docker.
set -u
PROJET=flyc
INSTALL_DIR=${1:-/opt/flyc}

if ! command -v docker >/dev/null 2>&1; then
  echo "docker_dedie=non"
  echo "docker absent, rien à faire"
  exit 0
fi

# 1. Ressources du projet Flyc uniquement (conteneurs, volumes, réseau du compose).
if [ -f "$INSTALL_DIR/docker-compose.yml" ]; then
  docker compose --project-directory "$INSTALL_DIR" down -v --remove-orphans >/dev/null 2>&1 || true
fi
mapfile -t flyc_ct < <(docker ps -aq --filter "label=com.docker.compose.project=$PROJET" 2>/dev/null)
if [ ${#flyc_ct[@]} -gt 0 ]; then
  docker rm -f "${flyc_ct[@]}" >/dev/null 2>&1 || true
  echo "${#flyc_ct[@]} conteneur(s) Flyc supprimé(s)"
fi

# 2. Reste-t-il des conteneurs ou des images qui ne sont pas à nous ?
autres=$(docker ps -aq 2>/dev/null | wc -l | tr -d ' ')
if [ "${autres:-0}" -gt 0 ]; then
  echo "docker_dedie=non"
  echo "$autres conteneur(s) étranger(s) à Flyc : Docker est conservé tel quel"
  exit 0
fi

# 3. Docker ne sert plus qu'à Flyc : on peut l'arrêter et laisser le playbook effacer ses données.
echo "docker_dedie=oui"
systemctl stop docker.socket docker containerd 2>/dev/null || true
# Crochets : sans eux le motif correspondrait à la ligne de commande de ce script, qui se
# tuerait lui-même.
pkill -f '[c]ontainerd-shim' 2>/dev/null || true
# Tuer le shim n'emporte pas le processus du conteneur : on vise ce qui reste dans un cgroup
# Docker, seul repère fiable une fois le démon parti.
killed=0
for p in /proc/[0-9]*; do
  pid=${p#/proc/}
  [ "$pid" = "$$" ] && continue
  if grep -qsE 'docker|containerd' "$p/cgroup" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null && killed=$((killed + 1))
  fi
done
[ "$killed" -gt 0 ] && echo "$killed processus de conteneur terminé(s)"
sleep 2
# Les montages (overlay, shm) feraient échouer la suppression sur « Device or resource busy ».
awk '$2 ~ "^/var/lib/docker" {print $2}' /proc/mounts | sort -r | while read -r m; do
  umount -l "$m" 2>/dev/null || true
done
ip link delete docker0 2>/dev/null || true
exit 0
