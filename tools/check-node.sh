#!/usr/bin/env bash
# Vérifie qu'un nœud ajouté est en service et connu de toute la plateforme.
#   tools/check-node.sh [-i inventaire] lb|queue <nom>
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INV="${FLYC_INVENTORY:-$ROOT/inventory/test.yml}"
[ "${1:-}" = "-i" ] && { INV="$2"; shift 2; }
kind="${1:-}"; name="${2:-}"
{ [ -n "$kind" ] && [ -n "$name" ]; } || { echo "usage : tools/check-node.sh [-i inventaire] lb|queue <nom>" >&2; exit 2; }

inv() { ansible-inventory -i "$INV" "$@" 2>/dev/null; }
DATA=$(inv --list | python3 -c 'import sys,json
try: print(json.load(sys.stdin)["data"]["hosts"][0])
except Exception: print("")')
[ -n "$DATA" ] || { echo "inventaire illisible ou sans groupe data : $INV" >&2; exit 2; }
inv --host "$name" >/dev/null || { echo "$name absent de l'inventaire $INV" >&2; exit 2; }
EDGES=$(inv --list | python3 -c 'import sys,json; print(" ".join(json.load(sys.stdin)["edge"]["hosts"]))')
IP=$(inv --host "$name" | python3 -c 'import sys,json
try: print(json.load(sys.stdin)["mgmt_ip"])
except Exception: print("")')
# Les variables de groupe sont fusionnees dans les variables de chaque hote
read -r VIP_WAIT WAIT_HOST <<EOF
$(inv --host "$name" | python3 -c 'import sys,json
d = json.load(sys.stdin)
print(d.get("flyc_vip_wait", {}).get("v4", "-"), d.get("flyc_wait_host", "-"))')
EOF

ok=0; ko=0
say() { if [ "$1" = ok ]; then printf '  \033[32m✓\033[0m %s\n' "$2"; ok=$((ok+1)); else printf '  \033[31m✗\033[0m %s\n' "$2"; ko=$((ko+1)); fi; }
sh_on() { ansible -i "$INV" "$1" -b -m ansible.builtin.shell -a "$2" 2>/dev/null | tail -n +2 | tr -d '\r'; }
# Clé plateforme et mots de passe sont lus sur les hôtes ; rien ne transite par la ligne de commande.
ctl() { sh_on "$DATA" "curl -s -m 10 http://127.0.0.1:8090$1 -H \"Authorization: Bearer \$(grep -o 'flyc_[0-9a-f]*' /var/lib/flyc/control/initial-platform-api-key.txt | head -1)\""; }
# Statut d'un serveur dans la page de statistiques du LB (CSV : 1=backend, 2=serveur, 18=statut)
srv_status() { sh_on "$1" "curl -s 'http://127.0.0.1:8404/stats;csv' | awk -F, '\$1==\"$2\" && \$2==\"$3\" {print \$18}'" | tr -d '[:space:]'; }

if [ "$kind" = lb ]; then
  for s in haproxy bird dataplaneapi; do
    [ "$(sh_on "$name" "systemctl is-active $s")" = active ] && say ok "$s actif" || say ko "$s non actif"
  done
  b=$(sh_on "$name" "birdc show protocols | awk '\$2==\"BGP\"{print \$6}' | sort -u")
  [ "$b" = Established ] && say ok "sessions BGP établies" || say ko "BGP : ${b:-aucune session}"
  v=$(sh_on "$name" "ip -o addr show lo | grep -vcE '127.0.0.1|::1'")
  [ "${v:-0}" -gt 0 ] 2>/dev/null && say ok "$v VIP portées sur lo" || say ko "aucune VIP sur lo"
  # /healthz n'est servi que sur la VIP de la salle d'attente et pour son nom d'hote
  h=$(sh_on "$name" "curl -sk -o /dev/null -w '%{http_code}' -H 'Host: $WAIT_HOST' https://$VIP_WAIT/healthz")
  [ "$h" = 200 ] && say ok "la salle d'attente repond en HTTPS sur la VIP" || say ko "VIP wait : reponse ${h:-aucune}"
  # La synchronisation est asynchrone (boucle de 60 s) : on laisse le temps de converger.
  for _ in 1 2 3 4 5 6; do
    st=$(ctl /v1/edges | python3 -c "import sys,json
try:
    e=[x for x in json.load(sys.stdin) if x['name']=='$name']
    print('absent' if not e else ('sync' if e[0]['in_sync'] else 'desync: '+(e[0].get('last_error') or '')))
except Exception: print('injoignable')")
    [ "$st" = sync ] && break
    sleep 10
  done
  [ "$st" = sync ] && say ok "connu de flyc-control et synchronisé" || say ko "flyc-control : $st"
else
  [ "$(sh_on "$name" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz")" = 200 ] \
    && say ok "flyc-queue répond 200 sur /healthz" || say ko "flyc-queue ne répond pas"
  rc="docker exec flyc-redis-a redis-cli -a \$(sed -n 's/^REDIS_PASSWORD=//p' /opt/flyc/.env) --no-auth-warning -p 7000"
  [ "$(sh_on "$name" "$rc cluster info | grep -c cluster_state:ok")" = 1 ] \
    && say ok "nœud dans un cluster Redis en état ok" || say ko "cluster Redis non ok depuis ce nœud"
  m=$(sh_on "$name" "$rc cluster nodes | grep -c '^.* $IP:'")
  [ "${m:-0}" -ge 2 ] 2>/dev/null && say ok "ses $m nœuds Redis sont membres du cluster" || say ko "${m:-0} nœud Redis membre du cluster (2 attendus)"
  for _ in 1 2 3 4 5 6; do
    st=$(ctl /v1/queue-hosts | python3 -c "import sys,json
try:
    h=[x for x in json.load(sys.stdin) if x['name']=='$name']
    print('absent' if not h else h[0]['health'])
except Exception: print('injoignable')")
    [ "$st" = ok ] && break
    sleep 10
  done
  [ "$st" = ok ] && say ok "connu de flyc-control" || say ko "flyc-control : $st"
fi

for lb in $EDGES; do
  if [ "$kind" = queue ]; then
    s=$(srv_status "$lb" bk_flyc_queue "$name")
    [ "$s" = UP ] && say ok "$lb : serveur $name UP dans bk_flyc_queue" || say ko "$lb : $name ${s:-absent} dans bk_flyc_queue"
  else
    [ "$lb" = "$name" ] && continue
    p=$(sh_on "$lb" "grep -c '^  peer $name ' /etc/haproxy/haproxy.cfg")
    [ "${p:-0}" = 1 ] && say ok "$lb : $name dans la section peers" || say ko "$lb : $name absent de la section peers"
  fi
done

printf '\n  %d vérification(s) ok, %d en échec\n' "$ok" "$ko"
[ "$ko" -eq 0 ]
