#!/usr/bin/env bash
# Retire les règles ufw posées par Flyc, et seulement celles-là.
#
# Un « ufw --force reset » effacerait aussi les règles de l'administrateur et couperait des
# services sans rapport sur une machine partagée. Les règles de Flyc portent le commentaire
# « flyc » : on les retire une à une et on ne touche ni à la politique par défaut, ni à l'état
# activé du pare-feu.
set -u
command -v ufw >/dev/null 2>&1 || { echo "ufw absent"; exit 0; }
n=0
while read -r rule; do
  [ -n "$rule" ] || continue
  # shellcheck disable=SC2086
  ufw delete $rule >/dev/null 2>&1 && n=$((n + 1))
done < <(ufw show added 2>/dev/null | sed -n "s/^ufw \(.*\) comment 'flyc'\$/\1/p")
echo "$n règle(s) Flyc retirée(s) ; les règles de l'administrateur et l'état du pare-feu sont inchangés"
