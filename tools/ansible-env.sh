# Accélérateur optionnel : si Mitogen est installé dans .ansible/mitogen, le playbook l'utilise.
# Mitogen garde une connexion et un interpréteur Python par hôte au lieu d'en relancer un à chaque
# tâche ; sur ce playbook, environ 20 % de temps en moins. Rien n'en dépend : sans lui tout marche.
#
#   make ansible-fast     installe Mitogen dans le dépôt (aucune modification du Python du système)
#   source tools/ansible-env.sh    avant ansible-playbook, pour en profiter à la main
_flyc_root="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
if [ -d "$_flyc_root/.ansible/mitogen/ansible_mitogen" ]; then
  export PYTHONPATH="$_flyc_root/.ansible/mitogen${PYTHONPATH:+:$PYTHONPATH}"
  export ANSIBLE_STRATEGY_PLUGINS="$_flyc_root/.ansible/mitogen/ansible_mitogen/plugins/strategy"
  export ANSIBLE_STRATEGY=mitogen_linear
  export FLYC_PLAY_STRATEGY=mitogen_free   # variante « free » du play parallèle (voir edge/site.yml)
fi
unset _flyc_root
