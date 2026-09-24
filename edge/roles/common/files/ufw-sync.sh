#!/usr/bin/env bash
# Synchronise en un seul passage les règles ufw gérées par Flyc avec l'état voulu.
#
# Les règles voulues arrivent sur l'entrée standard, une par ligne, en arguments ufw :
#   allow 443/tcp
#   allow from 10.0.0.5 to any port 5555 proto tcp
#
# Sont gérées les règles portant le commentaire « flyc », plus celles qui visent un port que Flyc
# gère sur cet hôte (héritage de l'ancienne méthode, ou nœud retiré depuis) : elles sont adoptées
# puis supprimées si elles ne sont plus voulues. L'accès SSH n'est jamais concerné, il n'apparaît
# pas dans les règles voulues. Ajouter une règle déjà présente dont seul le commentaire diffère la
# met à jour en place : il n'y a aucun instant où le port serait fermé.
#
# Sortie : une ligne par changement, vide si rien n'a bougé (Ansible s'en sert pour « changed »).
set -euo pipefail
TAG=flyc

mapfile -t want < <(grep -vE '^[[:space:]]*(#.*)?$' | sed 's/[[:space:]]*$//' | sort -u)
[ ${#want[@]} -gt 0 ] || { echo "aucune règle voulue reçue : refus de tout supprimer" >&2; exit 1; }

# Ports gérés par Flyc sur cet hôte, deduits des règles voulues.
mapfile -t ports < <(printf '%s\n' "${want[@]}" | sed -n 's/.* to any port \([0-9]*\) proto tcp$/\1/p' | sort -u)

in_list() { local n="$1"; shift; local x; for x in "$@"; do [ "$x" = "$n" ] && return 0; done; return 1; }
rules_tagged() { ufw show added 2>/dev/null | sed -n "s/^ufw \(.*\) comment '$TAG'\$/\1/p" | sort -u; }

changes=()

# 1. Adoption des règles héritées : même forme, port géré par Flyc, mais sans le marqueur.
while read -r line; do
  [ -n "$line" ] || continue
  p=$(printf '%s' "$line" | sed -n 's/.* to any port \([0-9]*\) proto tcp$/\1/p')
  [ -n "$p" ] || continue
  in_list "$p" ${ports[@]+"${ports[@]}"} || continue
  # shellcheck disable=SC2086
  ufw $line comment "$TAG" >/dev/null
done < <(ufw show added 2>/dev/null | sed -n "/comment '$TAG'\$/!s/^ufw //p")

mapfile -t have < <(rules_tagged)

# 2. Ajouts (et marquage des règles voulues déjà présentes sans commentaire).
for r in ${want[@]+"${want[@]}"}; do
  in_list "$r" ${have[@]+"${have[@]}"} && continue
  # shellcheck disable=SC2086
  ufw $r comment "$TAG" >/dev/null
  changes+=("+ $r")
done

# 3. Retraits : règles gérées qui ne sont plus voulues (nœud retiré, port changé).
for r in ${have[@]+"${have[@]}"}; do
  in_list "$r" ${want[@]+"${want[@]}"} && continue
  # shellcheck disable=SC2086
  ufw delete $r >/dev/null
  changes+=("- $r")
done

# 4. Politique par defaut et activation, dans le meme passage (un appel ufw de moins par hote).
if ! ufw status verbose 2>/dev/null | grep -q '^Default: deny (incoming)'; then
  ufw default deny incoming >/dev/null
  changes+=("politique par defaut : deny incoming")
fi
if ! ufw status 2>/dev/null | head -1 | grep -q 'Status: active'; then
  ufw --force enable >/dev/null
  changes+=("pare-feu active")
fi

printf '%s\n' ${changes[@]+"${changes[@]}"}
