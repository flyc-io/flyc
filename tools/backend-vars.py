#!/usr/bin/env python3
"""Construit les variables d'appel de l'API control pour declarer un backend client.

  backend-vars.py <tenant> <nom> <ip:port> [ip:port...]

Sortie : un objet JSON a passer tel quel a ansible-playbook -e.
"""
import json
import sys

if len(sys.argv) < 4:
    sys.exit("usage : backend-vars.py <tenant> <nom> <ip:port>...")
tenant, name, *servers = sys.argv[1:]
out = []
for i, s in enumerate(servers, 1):
    host, _, port = s.rpartition(":")
    if not host or not port.isdigit():
        sys.exit(f"serveur invalide : {s} (attendu ip:port)")
    out.append({"name": f"s{i}", "address": host, "port": int(port), "weight": 1, "maxconn": 10000})
print(json.dumps({
    "api_method": "POST",
    "api_path": "/v1/backends",
    "api_tenant": tenant,
    "api_body": {"name": name, "balance": "roundrobin", "check_path": "/", "servers": out},
}))
