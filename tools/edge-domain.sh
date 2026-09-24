#!/usr/bin/env bash
# Déclare un domaine protégé sur tous les LB via la Dataplane API (en attendant flyc-control, jalon M3).
# Usage : DP_USER=flyc DP_PASS=... tools/edge-domain.sh add    <host> <room-id> <backend ip:port> [edge-url ...]
#         DP_USER=flyc DP_PASS=... tools/edge-domain.sh state  <room-id> open|queue|closed         [edge-url ...]
#         DP_USER=flyc DP_PASS=... tools/edge-domain.sh remove <host> <room-id>                     [edge-url ...]
# edge-url : http://<mgmt_ip>:5555 (par défaut : lus dans $EDGES, séparés par des espaces)
set -euo pipefail
: "${DP_USER:?DP_USER manquant}" "${DP_PASS:?DP_PASS manquant}"
cmd="${1:?commande}"; shift
edges_from_args() { EDGE_LIST=("$@"); if [ ${#EDGE_LIST[@]} -eq 0 ]; then read -r -a EDGE_LIST <<< "${EDGES:-}"; fi; [ ${#EDGE_LIST[@]} -gt 0 ] || { echo "aucun edge : passer les URL en argument ou dans \$EDGES" >&2; exit 2; }; }
api() { local edge="$1" method="$2" path="$3"; shift 3; curl -sS -u "$DP_USER:$DP_PASS" -X "$method" "$edge/v3$path" -H 'Content-Type: application/json' "$@"; }
version() { api "$1" GET /services/haproxy/configuration/version; }
map_set() { # edge map key value  (runtime + persisté dans le fichier ; API v3 : /runtime/maps/{map}/entries)
  local edge="$1" map="$2" key="$3" val="$4"
  if api "$edge" GET "/services/haproxy/runtime/maps/$map/entries/$key" -o /dev/null -w '%{http_code}' | grep -q '^200$'; then
    api "$edge" PUT "/services/haproxy/runtime/maps/$map/entries/$key?force_sync=true" -d "{\"value\":\"$val\"}" >/dev/null
  else
    api "$edge" POST "/services/haproxy/runtime/maps/$map/entries?force_sync=true" -d "{\"key\":\"$key\",\"value\":\"$val\"}" >/dev/null
  fi
}
map_del() { api "$1" DELETE "/services/haproxy/runtime/maps/$2/entries/$3?force_sync=true" >/dev/null || true; }
backend_name() { echo "bk_$(echo "$1" | tr -c 'a-zA-Z0-9\n' '_')"; }

case "$cmd" in
  add)
    host="${1:?host}"; room="${2:?room}"; target="${3:?ip:port}"; shift 3
    bk=$(backend_name "$host"); edges_from_args "$@"
    for edge in "${EDGE_LIST[@]}"; do
      echo "== $edge"
      v=$(version "$edge")
      if api "$edge" GET "/services/haproxy/configuration/backends/$bk" -o /dev/null -w '%{http_code}' | grep -q 200; then
        echo "   backend $bk existe"
      else
        api "$edge" POST "/services/haproxy/configuration/backends?version=$v" \
          -d "{\"name\":\"$bk\",\"mode\":\"http\",\"balance\":{\"algorithm\":\"roundrobin\"},\"adv_check\":\"httpchk\",\"httpchk_params\":{\"method\":\"GET\",\"uri\":\"/\"},\"default_server\":{\"check\":\"enabled\",\"inter\":5000,\"rise\":2,\"fall\":3}}" >/dev/null
        v=$(version "$edge")
        api "$edge" POST "/services/haproxy/configuration/backends/$bk/servers?version=$v" \
          -d "{\"name\":\"srv1\",\"address\":\"${target%:*}\",\"port\":${target##*:}}" >/dev/null
        echo "   backend $bk → $target créé (reload)"
      fi
      map_set "$edge" rooms.map "$host" "$room"
      map_set "$edge" backends.map "$host" "$bk"
      map_set "$edge" states.map "$room" "queue"
      echo "   maps : $host → room=$room backend=$bk state=queue"
    done ;;
  state)
    room="${1:?room}"; st="${2:?open|queue|closed}"; shift 2; edges_from_args "$@"
    for edge in "${EDGE_LIST[@]}"; do map_set "$edge" states.map "$room" "$st"; echo "== $edge : $room → $st"; done ;;
  remove)
    host="${1:?host}"; room="${2:?room}"; shift 2; edges_from_args "$@"
    for edge in "${EDGE_LIST[@]}"; do map_del "$edge" rooms.map "$host"; map_del "$edge" backends.map "$host"; map_del "$edge" states.map "$room"; echo "== $edge : $host retiré (backend conservé)"; done ;;
  *) echo "commande inconnue : $cmd" >&2; exit 2 ;;
esac
