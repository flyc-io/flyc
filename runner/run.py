#!/usr/bin/env python3
"""Exécute les tâches déposées par flyc-control dans le répertoire d'échange.

Control décide, le runner exécute, et les deux ne se parlent que par des fichiers. Ce choix n'est
pas de la simplicité pour la simplicité : un déploiement redémarre flyc-control (il se déploie
lui-même), et un protocole de fichiers survit à ce redémarrage là où une connexion ne survivrait
pas. Il évite aussi d'exposer un service réseau de plus, donc une authentification de plus.

Cycle de vie d'une tâche, dans FLYC_JOBS_DIR :

    <id>.request.json   déposé par control     ce qu'il faut exécuter
    <id>.claim          créé par le runner     empêche une double exécution
    <id>.log            écrit au fil de l'eau  suivi en direct par control (SSE)
    <id>.result.json    écrit à la fin         code de sortie et durée

Une seule tâche à la fois, volontairement : ces opérations touchent la même plateforme.
"""
import json
import os
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

JOBS_DIR = Path(os.environ.get("FLYC_JOBS_DIR", "/var/lib/flyc/jobs"))
STATE_DIR = Path(os.environ.get("FLYC_RUNNER_DIR", "/var/lib/flyc/runner"))
REPO = Path(os.environ.get("FLYC_REPO_DIR", "/opt/flyc"))
DEPLOY_KEY = Path(os.environ.get("FLYC_DEPLOY_KEY", "/var/lib/flyc/deploy/id_ed25519"))
POLL_S = float(os.environ.get("FLYC_RUNNER_POLL", "1"))

# Drapeau posé par runner-refresh.sh sur l'hôte : un remplacement de ce conteneur attend la fin de
# la tâche en cours et ne doit pas en voir démarrer une autre entre-temps. Le script rafraîchit la
# date du fichier tant qu'il vit ; passé ce délai, le drapeau est un oubli et n'est plus respecté.
PAUSE = JOBS_DIR / ".paused"
PAUSE_MAX_S = 120

# Control est de confiance — il peut de toute façon tout faire via Ansible — mais n'accepter que
# des points d'entrée connus transforme un bug de sérialisation en refus net plutôt qu'en
# exécution arbitraire.
PLAYBOOKS = {
    "edge/site.yml",
    "tools/propagate-node.yml",
    "tools/control-api.yml",
    "tools/redis-detach.yml",
    "tools/decommission-node.yml",
}
SCRIPTS = {"tools/add-node.sh", "tools/check-node.sh"}

_current = None          # processus en cours, pour lui transmettre un arrêt demandé
_stopping = False


def now():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def log(msg):
    print(f"{now()} flyc-runner: {msg}", flush=True)


def claim(job_id):
    """Marque la tâche comme prise. Renvoie False si elle l'était déjà."""
    try:
        fd = os.open(JOBS_DIR / f"{job_id}.claim", os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o640)
    except FileExistsError:
        return False
    os.write(fd, now().encode())
    os.close(fd)
    return True


def write_result(job_id, rc, started, detail="", elapsed=None):
    tmp = JOBS_DIR / f"{job_id}.result.tmp"
    payload = {"id": job_id, "rc": rc, "started_at": started, "finished_at": now(), "detail": detail}
    if elapsed is not None:
        payload["duration_s"] = round(elapsed, 1)
    tmp.write_text(json.dumps(payload))
    # Renommage atomique : control ne voit jamais un résultat à moitié écrit.
    tmp.replace(JOBS_DIR / f"{job_id}.result.json")


def build_command(req, inventory_path):
    kind = req.get("kind", "playbook")
    if kind == "playbook":
        pb = req.get("playbook", "")
        if pb not in PLAYBOOKS:
            raise ValueError(f"playbook non autorisé : {pb!r}")
        cmd = ["ansible-playbook", "-i", str(inventory_path), pb]
        if req.get("limit"):
            cmd += ["--limit", req["limit"]]
        if req.get("tags"):
            cmd += ["--tags", req["tags"]]
        if req.get("check"):
            cmd += ["--check", "--diff"]
        for key, value in (req.get("extra_vars") or {}).items():
            # Un bloc JSON par variable : « -e clé=valeur » serait découpé sur les espaces.
            cmd += ["-e", json.dumps({key: value})]
        return cmd
    if kind == "script":
        argv = req.get("argv") or []
        if not argv or argv[0] not in SCRIPTS:
            raise ValueError(f"script non autorisé : {argv[:1]}")
        return [str(REPO / argv[0])] + argv[1:]
    raise ValueError(f"type de tâche inconnu : {kind!r}")


def environment(inventory_path):
    env = dict(os.environ)
    env.update({
        "PYTHONUNBUFFERED": "1",
        "ANSIBLE_FORCE_COLOR": "0",
        "ANSIBLE_HOST_KEY_CHECKING": "False",
        "ANSIBLE_RETRY_FILES_ENABLED": "False",
        # Écriture impossible dans l'image en lecture seule : tout l'éphémère va dans le volume.
        "ANSIBLE_LOCAL_TEMP": str(STATE_DIR / "tmp"),
        "ANSIBLE_SSH_CONTROL_PATH_DIR": str(STATE_DIR / "cp"),
        "HOME": str(STATE_DIR),
        "FLYC_INVENTORY": str(inventory_path),
    })
    if DEPLOY_KEY.exists():
        env["ANSIBLE_PRIVATE_KEY_FILE"] = str(DEPLOY_KEY)
    return env


def run_job(job_id, req):
    started, t0 = now(), time.monotonic()
    logfile = JOBS_DIR / f"{job_id}.log"
    inventory_path = STATE_DIR / f"{job_id}.inventory.yml"
    try:
        inventory_path.write_text(req.get("inventory") or "")
        cmd = build_command(req, inventory_path)
    except Exception as exc:                       # requête malformée : échec net et lisible
        logfile.write_text(f"requête invalide : {exc}\n")
        write_result(job_id, 2, started, str(exc), time.monotonic() - t0)
        return

    global _current
    with logfile.open("a", buffering=1) as out:
        out.write(f"$ {' '.join(cmd)}\n\n")
        try:
            _current = subprocess.Popen(cmd, cwd=str(REPO), env=environment(inventory_path),
                                        stdout=out, stderr=subprocess.STDOUT, start_new_session=True)
            rc = _current.wait()
        except FileNotFoundError as exc:
            out.write(f"\ncommande introuvable : {exc}\n")
            rc = 127
        finally:
            _current = None
        out.write(f"\n-- terminé, code {rc} --\n")
    inventory_path.unlink(missing_ok=True)
    write_result(job_id, rc, started, elapsed=time.monotonic() - t0)
    log(f"tâche {job_id} terminée, code {rc}")


def pending():
    """Tâches à traiter, les plus anciennes d'abord."""
    out = []
    for req in sorted(JOBS_DIR.glob("*.request.json"), key=lambda p: p.stat().st_mtime):
        job_id = req.name[: -len(".request.json")]
        if (JOBS_DIR / f"{job_id}.result.json").exists():
            continue
        out.append((job_id, req))
    return out


def sweep_interrupted():
    """Une tâche prise mais sans résultat vient d'un runner interrompu : on ne la rejoue pas."""
    for claim_file in JOBS_DIR.glob("*.claim"):
        job_id = claim_file.name[: -len(".claim")]
        if (JOBS_DIR / f"{job_id}.result.json").exists():
            continue
        log(f"tâche {job_id} interrompue par un arrêt du runner")
        with (JOBS_DIR / f"{job_id}.log").open("a", buffering=1) as out:
            out.write("\n-- le runner a été interrompu, tâche non terminée --\n")
        write_result(job_id, 75, now(), "runner interrompu")


def en_pause():
    try:
        age = time.time() - PAUSE.stat().st_mtime
    except FileNotFoundError:
        return False
    if age > PAUSE_MAX_S:
        log(f"drapeau de pause vieux de {int(age)}s, ignoré")
        return False
    return True


def on_signal(signum, _frame):
    global _stopping
    _stopping = True
    log(f"signal {signum} reçu, arrêt après la tâche en cours")
    if _current is not None:
        _current.terminate()


def once(argv):
    """Exécution unique, sans tâche ni journal : « flyc-runner once -i <inventaire> <playbook> … ».

    C'est le mode d'amorçage. tools/install-control.sh l'emploie pour faire jouer edge/site.yml sur
    la machine qui devient l'hôte control, avant que control — et donc le protocole de tâches —
    n'existe. La sortie va au terminal de l'opérateur, qui est là pour la lire.
    """
    if len(argv) < 3 or argv[0] != "-i":
        print("usage : flyc-runner once -i <inventaire> <playbook> [options ansible]", file=sys.stderr)
        return 2
    inventory_path, playbook, reste = argv[1], argv[2], argv[3:]
    if playbook not in PLAYBOOKS:
        print(f"playbook non autorisé : {playbook!r}", file=sys.stderr)
        return 2
    for d in (STATE_DIR, STATE_DIR / "tmp", STATE_DIR / "cp"):
        d.mkdir(parents=True, exist_ok=True)
    cmd = ["ansible-playbook", "-i", inventory_path, playbook] + reste
    log(f"amorçage : {' '.join(cmd)}")
    return subprocess.call(cmd, cwd=str(REPO), env=environment(Path(inventory_path)))


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "once":
        return once(sys.argv[2:])
    for d in (JOBS_DIR, STATE_DIR, STATE_DIR / "tmp", STATE_DIR / "cp"):
        d.mkdir(parents=True, exist_ok=True)
    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)
    log(f"démarré, répertoire des tâches {JOBS_DIR}, dépôt {REPO}")
    sweep_interrupted()
    pause_signalee = False
    while not _stopping:
        if en_pause():
            if not pause_signalee:
                log("remplacement du conteneur en attente : aucune nouvelle tâche prise")
                pause_signalee = True
            time.sleep(POLL_S)
            continue
        if pause_signalee:
            log("pause levée")
            pause_signalee = False
        for job_id, req_file in pending():
            if not claim(job_id):
                continue
            try:
                req = json.loads(req_file.read_text())
            except Exception as exc:
                write_result(job_id, 2, now(), f"requête illisible : {exc}")
                continue
            log(f"tâche {job_id} : {req.get('kind', 'playbook')} {req.get('playbook') or req.get('argv')}")
            run_job(job_id, req)
            if _stopping:
                break
        time.sleep(POLL_S)
    log("arrêté")
    return 0


if __name__ == "__main__":
    sys.exit(main())
