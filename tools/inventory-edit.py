#!/usr/bin/env python3
"""Ajoute ou retire un hôte dans un groupe de l'inventaire, en préservant commentaires et mise en forme.

  inventory-edit.py add    <inventaire> <groupe> <nom> <clé=valeur> [clé=valeur…]
  inventory-edit.py remove <inventaire> <groupe> <nom>
  inventory-edit.py list   <inventaire> [groupe]

Sortie : la liste des hôtes du groupe après opération. Code 3 si rien à faire (déjà dans l'état voulu).
"""
import re
import sys


def load(path):
    with open(path, encoding="utf-8") as f:
        return f.read().split("\n")


def group_span(lines, group):
    """Renvoie (début_hosts, fin_hosts, indentation) pour les lignes d'hôtes du groupe."""
    ci = next((i for i, l in enumerate(lines) if l.strip() == "children:"), None)
    if ci is None:
        sys.exit("inventaire sans section children:")
    base = len(lines[ci]) - len(lines[ci].lstrip())
    gi = None
    for i in range(ci + 1, len(lines)):
        l = lines[i]
        if not l.strip():
            continue
        ind = len(l) - len(l.lstrip())
        if ind <= base:            # fin du bloc children
            break
        if ind == base + 2 and re.match(rf"^\s+{re.escape(group)}:\s*$", l):
            gi = i
            break
    if gi is None:
        sys.exit(f"groupe '{group}' introuvable sous children:")
    hi = None
    for i in range(gi + 1, len(lines)):
        l = lines[i]
        if not l.strip():
            continue
        ind = len(l) - len(l.lstrip())
        if ind <= base + 2:        # groupe suivant, sans section hosts
            break
        if l.strip() == "hosts:":
            hi = i
            break
    if hi is None:
        sys.exit(f"le groupe '{group}' n'a pas de section hosts:")
    indent = len(lines[hi]) - len(lines[hi].lstrip()) + 2
    end = hi + 1
    while end < len(lines) and (not lines[end].strip() or lines[end].startswith(" " * indent)):
        end += 1
    while end > hi + 1 and not lines[end - 1].strip():
        end -= 1
    return hi + 1, end, indent


def hosts_in(lines, start, end):
    out = []
    for i in range(start, end):
        m = re.match(r"^\s+([A-Za-z0-9_.-]+)\s*:", lines[i])
        if m:
            out.append((m.group(1), i))
    return out


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    cmd, path = sys.argv[1], sys.argv[2]
    lines = load(path)

    if cmd == "list":
        groups = [sys.argv[3]] if len(sys.argv) > 3 else ["edge", "queue", "data"]
        for g in groups:
            s, e, _ = group_span(lines, g)
            print(f"{g}: " + " ".join(n for n, _ in hosts_in(lines, s, e)))
        return

    group, name = sys.argv[3], sys.argv[4]
    start, end, indent = group_span(lines, group)
    present = dict(hosts_in(lines, start, end))

    if cmd == "add":
        attrs = dict(a.split("=", 1) for a in sys.argv[5:])
        entry = " ".join(f"{k}: \"{v}\"," for k, v in attrs.items()).rstrip(",")
        line = f"{' ' * indent}{name}: {{ {entry} }}"
        if name in present:
            if lines[present[name]].rstrip() == line:
                print(f"{name} déjà présent à l'identique dans {group}", file=sys.stderr)
                sys.exit(3)
            lines[present[name]] = line
        else:
            lines.insert(end, line)
    elif cmd == "remove":
        if name not in present:
            print(f"{name} absent de {group}", file=sys.stderr)
            sys.exit(3)
        del lines[present[name]]
    else:
        sys.exit(f"commande inconnue : {cmd}")

    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    s, e, _ = group_span(lines, group)
    print(f"{group}: " + " ".join(n for n, _ in hosts_in(lines, s, e)))


if __name__ == "__main__":
    main()
