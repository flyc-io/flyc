#!/usr/bin/env python3
"""Remet la topologie du cluster Redis dans sa forme voulue : un maître par machine, et la
réplique de chaque maître sur une autre machine que lui.

Deux réparations, dans cet ordre.

1. Répartition des maîtres. Après la panne d'une machine, sa réplique est promue ailleurs et la
   machine revenue redevient simple réplique : un hôte se retrouve avec deux maîtres, un autre
   avec aucun. Le service continue, mais cet hôte porte deux fois plus de charge et sa perte
   ferait basculer deux shards d'un coup. Une bascule volontaire (CLUSTER FAILOVER, coordonnée
   et sans perte) rend son rôle de maître à la machine revenue.

2. Placement des répliques : chaque maître doit avoir exactement une réplique, sur une autre
   machine que lui. Sinon la perte d'un hôte emporte un maître et sa réplique, donc ses slots.

Deux pièges justifient ce script plutôt qu'un simple « attache la réplique ailleurs » :

* laisser un maître sans réplique déclenche la migration automatique de Redis, qui lui en donne
  une — parfois celle du même hôte, défaisant le placement qu'on vient de faire ;
* la migration se produit quelques secondes après coup, donc une vérification immédiate conclut
  à tort que tout va bien.

On calcule donc une affectation complète (aucun maître orphelin), on l'applique avec la barrière
de migration relevée, on vérifie après stabilisation, puis on rétablit la barrière. L'état final
ne comportant aucun orphelin, Redis n'a plus de raison d'y toucher.

La sous-commande « detach » sort au contraire les nœuds d'une machine qui s'en va : elle vide
d'abord ses slots (opération en ligne qui déplace de vraies clés, quelques minutes par shard),
puis retire ses nœuds du cluster.

Usage :
  redis-topology.py balance <ip:port d'un nœud>
  redis-topology.py detach  <ip:port d'un nœud qui reste> <ip de la machine qui part>

Le mot de passe est lu dans /opt/flyc/.env.
Sortie : une ligne par correction, un message de constat si rien à faire.
"""
import re
import subprocess
import sys
import time

ENV = "/opt/flyc/.env"
SETTLE_S = 5


def password():
    with open(ENV, encoding="utf-8") as f:
        for line in f:
            if line.startswith("REDIS_PASSWORD="):
                return line.split("=", 1)[1].strip()
    sys.exit(f"REDIS_PASSWORD introuvable dans {ENV}")


def redis(pw, addr, *args):
    host, _, port = addr.rpartition(":")
    cmd = ["docker", "exec", "flyc-redis-a", "redis-cli", "-a", pw, "--no-auth-warning",
           "-h", host, "-p", port, *args]
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(f"redis-cli {' '.join(args)} sur {addr} : {out.stderr.strip()}")
    return out.stdout


class Node:
    __slots__ = ("id", "addr", "flags", "master", "slots")

    def __init__(self, fields):
        self.id = fields[0]
        self.addr = fields[1].split("@")[0]
        self.flags = fields[2]
        self.master = fields[3]
        self.slots = fields[8:] if len(fields) > 8 else []

    @property
    def host(self):
        return self.addr.rsplit(":", 1)[0]

    @property
    def is_master(self):
        return "master" in self.flags


def topology(pw, entry):
    nodes = [Node(l.split()) for l in redis(pw, entry, "cluster", "nodes").splitlines() if l.strip()]
    masters = sorted((n for n in nodes if n.is_master and n.slots), key=lambda n: n.addr)
    replicas = sorted((n for n in nodes if not n.is_master), key=lambda n: n.addr)
    return nodes, masters, replicas


def assign(masters, replicas):
    """Chaque réplique reçoit un maître, jamais sur son propre hôte, chaque maître au plus une.
    Les hôtes étant triés, la rotation d'un cran convient dès qu'un hôte porte un maître et une
    réplique ; on retombe sur une affectation gloutonne si la topologie est plus irrégulière."""
    n = min(len(masters), len(replicas))
    if n == 0:
        return {}
    rotated = {replicas[i].id: masters[(i + 1) % len(masters)] for i in range(n)}
    if all(replicas[i].host != rotated[replicas[i].id].host for i in range(n)):
        return rotated
    out, taken = {}, set()
    for r in replicas:
        cand = [m for m in masters if m.id not in taken and m.host != r.host]
        if not cand:
            continue
        out[r.id] = cand[0]
        taken.add(cand[0].id)
    return out


def rebalance_masters(pw, entry):
    """Rend un maître aux hôtes qui n'en ont plus, en basculant une de leurs répliques."""
    moved = 0
    for _ in range(10):                      # borne : une bascule par tour, topologie relue
        _, masters, replicas = topology(pw, entry)
        per_host = {}
        for m in masters:
            per_host.setdefault(m.host, []).append(m)
        idle = {r.host for r in replicas} - set(per_host)
        if not idle:
            return moved
        heavy = {h for h, ms in per_host.items() if len(ms) > 1}
        if not heavy:
            return moved
        by_id = {m.id: m for m in masters}
        cand = next((r for r in replicas
                     if r.host in idle and by_id.get(r.master) and by_id[r.master].host in heavy), None)
        if cand is None:
            return moved
        old = by_id[cand.master]
        redis(pw, cand.addr, "cluster", "failover")
        print(f"  {cand.addr} reprend le rôle de maître, {old.addr} redevient réplique")
        moved += 1
        time.sleep(SETTLE_S)
    return moved


MIN_MASTERS = 3          # un cluster Redis a besoin de trois maîtres pour rester cohérent
DRAIN_TIMEOUT_S = 1800


def cluster_ok(pw, entry):
    info = redis(pw, entry, "cluster", "info")
    return "cluster_state:ok" in info and "cluster_slots_ok:16384" in info


def detach(pw, entry, leaving_ip):
    """Vide puis retire du cluster les nœuds hébergés par leaving_ip."""
    if not cluster_ok(pw, entry):
        sys.exit("le cluster n'est pas sain : réparer avant de retirer un hôte")
    nodes, masters, replicas = topology(pw, entry)
    mine = [n for n in nodes if n.host == leaving_ip]
    if not mine:
        print(f"aucun nœud de {leaving_ip} dans le cluster : rien à faire")
        return
    if entry.rsplit(":", 1)[0] == leaving_ip:
        sys.exit("le point d'entrée est sur la machine qui part : en choisir un autre")
    staying = [m for m in masters if m.host != leaving_ip]
    if len(staying) < MIN_MASTERS:
        sys.exit(f"il ne resterait que {len(staying)} maître(s), minimum {MIN_MASTERS} : refus")

    # 1. Vidage des slots, poids nul : redis-cli répartit sur les maîtres restants.
    for m in (n for n in mine if n.is_master and n.slots):
        print(f"  vidage de {m.addr} ({sum(1 for _ in m.slots)} plage(s) de slots), "
              "quelques minutes, le service continue")
        subprocess.run(["docker", "exec", "flyc-redis-a", "redis-cli", "-a", pw, "--no-auth-warning",
                        "--cluster", "rebalance", entry, "--cluster-weight", f"{m.id}=0"],
                       capture_output=True, text=True)
        deadline = time.time() + DRAIN_TIMEOUT_S
        while True:
            still = [n for n in topology(pw, entry)[0] if n.id == m.id and n.slots]
            if not still:
                break
            if time.time() > deadline:
                sys.exit(f"{m.addr} a encore des slots après {DRAIN_TIMEOUT_S}s : vidage incomplet")
            time.sleep(5)
        print(f"  {m.addr} vidé")

    if not cluster_ok(pw, entry):
        sys.exit("couverture des slots incomplète après vidage : ne pas retirer les nœuds")

    # 2. Retrait : les répliques d'abord, elles ne portent rien.
    nodes, _, _ = topology(pw, entry)
    mine = [n for n in nodes if n.host == leaving_ip]
    for n in sorted(mine, key=lambda x: x.is_master):
        redis(pw, entry, "--cluster", "del-node", entry, n.id)
        print(f"  {n.addr} retiré du cluster")
    time.sleep(SETTLE_S)
    if [n for n in topology(pw, entry)[0] if n.host == leaving_ip]:
        sys.exit(f"des nœuds de {leaving_ip} sont encore membres")
    if not cluster_ok(pw, entry):
        sys.exit("le cluster n'est pas sain après le retrait")
    print(f"{len(mine)} nœud(s) de {leaving_ip} sortis, cluster sain")


def main():
    if len(sys.argv) >= 2 and sys.argv[1] == "detach":
        if len(sys.argv) != 4:
            sys.exit("usage : redis-topology.py detach <ip:port qui reste> <ip qui part>")
        detach(password(), sys.argv[2], sys.argv[3])
        return
    args = sys.argv[2:] if (len(sys.argv) >= 2 and sys.argv[1] == "balance") else sys.argv[1:]
    if len(args) != 1:
        sys.exit("usage : redis-topology.py balance <ip:port d un noeud du cluster>")
    entry, pw = args[0], password()
    rebalanced = rebalance_masters(pw, entry)
    _, masters, replicas = topology(pw, entry)
    target = assign(masters, replicas)
    moves = [(r, target[r.id]) for r in replicas if r.id in target and r.master != target[r.id].id]
    orphans = [m for m in masters if not any(t.id == m.id for t in target.values())]
    if not moves:
        if rebalanced:
            print(f"{rebalanced} bascule(s) de maître, répliques déjà bien placées")
        else:
            print("topologie déjà correcte : un maître par hôte, chaque réplique ailleurs")
        return
    if orphans:
        print("attention : %d maître(s) resteront sans réplique (pas assez de nœuds)"
              % len(orphans), file=sys.stderr)

    # La barrière empêche Redis de réagir à l'état transitoire pendant qu'on réaffecte.
    for m in masters:
        redis(pw, m.addr, "config", "set", "cluster-migration-barrier", "99")
    try:
        for r, m in moves:
            redis(pw, r.addr, "cluster", "replicate", m.id)
            print(f"  {r.addr} suit désormais {m.addr}")
        time.sleep(SETTLE_S)
        _, masters2, replicas2 = topology(pw, entry)
        by_id = {n.id: n for n in masters2}
        for r in replicas2:
            m = by_id.get(r.master)
            if m and m.host == r.host:
                sys.exit(f"échec : {r.addr} réplique encore un maître du même hôte ({m.addr})")
    finally:
        for m in masters:
            redis(pw, m.addr, "config", "set", "cluster-migration-barrier", "1")
    print(f"{rebalanced} bascule(s) de maître et {len(moves)} déplacement(s) de réplique : "
          "un maître par hôte, chaque réplique sur une autre machine")


if __name__ == "__main__":
    main()
