#!/usr/bin/env bash
# Flyc — installation de l'hôte control sur une machine Ubuntu vierge.
#
# C'est le seul geste qui se fait encore en ligne de commande : il pose Docker, le compte de
# déploiement et sa clé, puis fait jouer edge/site.yml par l'image runner sur cette machine. Tout
# le reste — déclarer les LB, les hôtes de file, les clients — se fait ensuite depuis l'interface.
#
#   curl -fsSL https://.../install-control.sh | sudo sh -s -- --control-host control.exemple.fr …
#
# Options :
#   --control-host <fqdn>     nom de l'interface d'administration        (obligatoire)
#   --wait-host <fqdn>        nom de la salle d'attente                  (obligatoire)
#   --image-tag <tag>         version des images Flyc         (défaut : latest = dernière version)
#   --registry <hôte/projet>  registre d'images            (défaut : ghcr.io/flyc-io)
#   --registry-ca <fichier>   certificat racine du registre, si privé
#   --from-source             construire les images ici et les servir depuis un registre local,
#                             au lieu de les tirer d'Internet (suppose un clone du dépôt)
#   --acme-email <adresse>    contact Let's Encrypt (vide = pas d'ACME)
#   --public-url <url>        URL publique de l'interface   (défaut : https://<control-host>)
#   --ip <adresse>            adresse d'administration de cette machine  (défaut : déduite)
#   --name <nom>              nom du nœud dans l'inventaire              (défaut : hostname -s)
#   -y                        ne pas demander confirmation
set -euo pipefail

CONTROL_HOST=""; WAIT_HOST=""; IMAGE_TAG=""; REGISTRY="ghcr.io/flyc-io"; FROM_SOURCE=false
REGISTRY_CA=""; ACME_EMAIL=""; PUBLIC_URL=""; IP=""; NODE_NAME=""; ASSUME_YES=false
DATA_DIR=/var/lib/flyc
DEPLOY_USER=flyc-deploy

die()  { printf '\033[31merreur :\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --control-host) CONTROL_HOST="${2:-}"; shift 2;;
    --wait-host)    WAIT_HOST="${2:-}"; shift 2;;
    --image-tag)    IMAGE_TAG="${2:-}"; shift 2;;
    --registry)     REGISTRY="${2:-}"; shift 2;;
    --registry-ca)  REGISTRY_CA="${2:-}"; shift 2;;
    --from-source)  FROM_SOURCE=true; shift;;
    --acme-email)   ACME_EMAIL="${2:-}"; shift 2;;
    --public-url)   PUBLIC_URL="${2:-}"; shift 2;;
    --ip)           IP="${2:-}"; shift 2;;
    --name)         NODE_NAME="${2:-}"; shift 2;;
    -y|--yes)       ASSUME_YES=true; shift;;
    -h|--help)      sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) die "option inconnue : $1";;
  esac
done

[ "$(id -u)" = 0 ] || die "à lancer en root (sudo)"
[ -n "$CONTROL_HOST" ] || die "--control-host est obligatoire"
[ -n "$WAIT_HOST" ] || die "--wait-host est obligatoire"
command -v apt-get >/dev/null || die "cette procédure vise Ubuntu (apt introuvable)"
[ -z "$REGISTRY_CA" ] || [ -f "$REGISTRY_CA" ] || die "certificat introuvable : $REGISTRY_CA"
ROOT="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd || true)"
if $FROM_SOURCE; then
  [ -n "$ROOT" ] && [ -f "$ROOT/queue/Dockerfile" ] \
    || die "--from-source construit les images depuis les sources : lancer le script depuis un
  clone du dépôt (tools/install-control.sh), pas par « curl | sh »."
  IMAGE_TAG="${IMAGE_TAG:-$(git -C "$ROOT" rev-parse --short=8 HEAD 2>/dev/null || echo local)}"
fi
IMAGE_TAG="${IMAGE_TAG:-latest}"

NODE_NAME="${NODE_NAME:-$(hostname -s)}"
PUBLIC_URL="${PUBLIC_URL:-https://$CONTROL_HOST}"
# L'adresse d'administration : celle par laquelle cette machine sort, donc celle par laquelle les
# autres nœuds la joindront. Lecture seule, aucune configuration réseau n'est touchée.
if [ -z "$IP" ]; then
  IP=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1) || true
fi
[ -n "$IP" ] || die "adresse d'administration indéterminable : la passer avec --ip"

step "Installation de l'hôte control"
info "machine      : $NODE_NAME ($IP)"
info "interface    : $PUBLIC_URL"
info "salle d'attente : $WAIT_HOST"
info "images       : $REGISTRY/flyc-*:$IMAGE_TAG"
info "seront posés : Docker, le compte $DEPLOY_USER et sa clé, Postgres, flyc-control, la supervision"
if ! $ASSUME_YES; then
  printf '\nContinuer ? [o/N] : '
  read -r reponse < /dev/tty
  case "$reponse" in o*|O*|y*|Y*) ;; *) die "abandon";; esac
fi

step "Paquets de base"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl gnupg openssh-server >/dev/null

# Le certificat doit être connu du magasin système (pour Ansible et curl) et de Docker, qui lit
# /etc/docker/certs.d/<hôte[:port]>/ca.crt sans redémarrage.
poser_ca() {
  install -D -m 0644 "$1" /usr/local/share/ca-certificates/flyc-registry-ca.crt
  update-ca-certificates >/dev/null
  install -D -m 0644 "$1" "/etc/docker/certs.d/${REGISTRY%%/*}/ca.crt"
  # Chemin visible aussi bien de l'hôte que du conteneur runner, qui monte $DATA_DIR.
  install -D -m 0644 "$1" "$DATA_DIR/registry-ca.pem"
  REGISTRY_CA="$DATA_DIR/registry-ca.pem"
  info "certificat posé dans le magasin système et dans /etc/docker/certs.d/${REGISTRY%%/*}/"
}

if [ -n "$REGISTRY_CA" ] && ! $FROM_SOURCE; then
  step "Certificat du registre"
  poser_ca "$REGISTRY_CA"
fi

step "Docker"
if ! command -v docker >/dev/null; then
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  # Sous-shell : /etc/os-release définit NAME, VERSION et d'autres noms très communs.
  codename=$(. /etc/os-release && echo "$VERSION_CODENAME")
  arch=$([ "$(uname -m)" = x86_64 ] && echo amd64 || echo arm64)
  echo "deb [arch=$arch signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $codename stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update -qq
  apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
  info "installé"
else
  info "déjà présent ($(docker --version))"
fi

# Construction locale : aucune image n'est tirée d'Internet, mais les autres nœuds doivent pouvoir
# les récupérer. Un registre minimal tourne donc sur l'hôte control, avec un certificat généré ici
# et distribué aux nœuds par l'inventaire (flyc_registry_ca) — le rôle docker sait le poser.
if $FROM_SOURCE; then
  step "Registre local et construction des images"
  REGISTRY="$IP:5000"
  CERT_DIR="$DATA_DIR/registry/certs"
  install -d -m 0755 "$DATA_DIR/registry/data" "$CERT_DIR"
  if [ ! -f "$CERT_DIR/registry.crt" ]; then
    # Docker valide le nom du serveur : l'adresse doit figurer en SAN, le CN ne suffit plus.
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
      -subj "/CN=$IP" -addext "subjectAltName=IP:$IP" \
      -keyout "$CERT_DIR/registry.key" -out "$CERT_DIR/registry.crt" >/dev/null 2>&1 \
      || die "génération du certificat du registre impossible"
    chmod 0600 "$CERT_DIR/registry.key"
    info "certificat du registre généré pour $IP (10 ans)"
  else
    info "certificat du registre déjà présent, conservé"
  fi
  poser_ca "$CERT_DIR/registry.crt"

  if [ -z "$(docker ps -q -f name='^flyc-registry$')" ]; then
    docker rm -f flyc-registry >/dev/null 2>&1 || true
    docker run -d --name flyc-registry --restart unless-stopped -p "$IP:5000:5000" \
      -v "$DATA_DIR/registry/data:/var/lib/registry" \
      -v "$CERT_DIR:/certs:ro" \
      -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/registry.crt \
      -e REGISTRY_HTTP_TLS_KEY=/certs/registry.key \
      registry:2 >/dev/null || die "démarrage du registre local impossible"
    info "registre local démarré sur $IP:5000"
  else
    info "registre local déjà en service"
  fi
  # Le registre met un instant à écouter ; sans cette attente le premier push échoue.
  for _ in $(seq 1 30); do
    curl -fsS "https://$IP:5000/v2/" >/dev/null 2>&1 && break
    sleep 1
  done

  for img in queue control runner; do
    info "construction de flyc-$img:$IMAGE_TAG"
    docker build -q -f "$ROOT/$img/Dockerfile" --build-arg VERSION="$IMAGE_TAG" \
      -t "$REGISTRY/flyc-$img:$IMAGE_TAG" "$ROOT" >/dev/null \
      || die "construction de flyc-$img en échec"
    docker push -q "$REGISTRY/flyc-$img:$IMAGE_TAG" >/dev/null \
      || die "publication de flyc-$img vers le registre local en échec"
  done
  info "trois images construites et publiées sur $REGISTRY"
fi

step "Compte de déploiement $DEPLOY_USER"
id "$DEPLOY_USER" >/dev/null 2>&1 || \
  useradd --system --shell /bin/bash --home-dir "/var/lib/$DEPLOY_USER" --create-home "$DEPLOY_USER"
passwd -l "$DEPLOY_USER" >/dev/null 2>&1 || true
cat > "/etc/sudoers.d/$DEPLOY_USER" <<EOF
# Écrit à l'installation, puis maintenu par Ansible (rôle common) : ne pas modifier à la main.
$DEPLOY_USER ALL=(root) NOPASSWD: ALL
Defaults:$DEPLOY_USER !requiretty
EOF
chmod 0440 "/etc/sudoers.d/$DEPLOY_USER"
visudo -cf "/etc/sudoers.d/$DEPLOY_USER" >/dev/null

# La clé est générée ici plutôt que par Ansible : il faut l'avoir autorisée avant de pouvoir se
# connecter à cette machine. Le rôle secrets la retrouvera (creates:) et ne la régénérera pas.
install -d -m 0700 "$DATA_DIR/deploy"
if [ ! -f "$DATA_DIR/deploy/id_ed25519" ]; then
  ssh-keygen -q -t ed25519 -N '' -C "flyc-deploy@$CONTROL_HOST" -f "$DATA_DIR/deploy/id_ed25519"
  info "paire de clés générée dans $DATA_DIR/deploy"
else
  info "paire de clés déjà présente, conservée"
fi
chmod 0644 "$DATA_DIR/deploy/id_ed25519.pub"
install -d -m 0700 -o "$DEPLOY_USER" -g "$DEPLOY_USER" "/var/lib/$DEPLOY_USER/.ssh"
printf 'from="%s",no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc %s\n' \
  "$IP" "$(cat "$DATA_DIR/deploy/id_ed25519.pub")" > "/var/lib/$DEPLOY_USER/.ssh/authorized_keys"
chmod 0600 "/var/lib/$DEPLOY_USER/.ssh/authorized_keys"
chown "$DEPLOY_USER:$DEPLOY_USER" "/var/lib/$DEPLOY_USER/.ssh/authorized_keys"
info "clé autorisée depuis $IP uniquement"

step "Inventaire d'installation"
INV="$DATA_DIR/bootstrap-inventory.yml"
{
  echo "# Écrit par tools/install-control.sh. Ne sert qu'à cette première installation : ensuite"
  echo "# l'inventaire vit dans la base de flyc-control, et l'interface en est le seul point d'entrée."
  echo "all:"
  echo "  vars:"
  echo "    flyc_env: prod"
  echo "    ansible_user: $DEPLOY_USER"
  echo "    ansible_become: true"
  echo "    flyc_wait_host: \"$WAIT_HOST\""
  echo "    flyc_control_host: \"$CONTROL_HOST\""
  echo "    flyc_control_public_url: \"$PUBLIC_URL\""
  echo "    flyc_acme_email: \"$ACME_EMAIL\""
  echo "    flyc_image_tag: \"$IMAGE_TAG\""
  echo "    flyc_registry: \"$REGISTRY\""
  echo "    flyc_deploy_from: \"$IP\""
  case "$PUBLIC_URL" in http://*) echo "    flyc_cookie_secure: false";; esac
  # « [ … ] && echo » serait la derniere commande de la ligne : sans certificat elle renvoie 1 et
  # set -e arreterait le script en plein milieu de l'inventaire.
  if [ -n "$REGISTRY_CA" ]; then echo "    flyc_registry_ca_file: \"$DATA_DIR/registry-ca.pem\""; fi
  echo "  children:"
  echo "    edge:"
  echo "      hosts: {}"
  echo "    queue:"
  echo "      hosts: {}"
  echo "    data:"
  echo "      hosts:"
  echo "        $NODE_NAME: { ansible_host: \"$IP\", mgmt_ip: \"$IP\" }"
} > "$INV"
chmod 0644 "$INV"
info "$INV"

step "Installation par l'image runner"
IMAGE="$REGISTRY/flyc-runner:$IMAGE_TAG"
docker pull -q "$IMAGE" >/dev/null || die "image $IMAGE introuvable (registre joignable ? certificat posé ?)"
# Réseau de l'hôte : le runner se connecte en SSH à cette machine par son adresse $IP, la seule
# que la clé autorise. Le dépôt et les playbooks sont dans l'image, à la version des binaires.
jouer() {
  docker run --rm --network host \
    -v "$DATA_DIR:$DATA_DIR" \
    -e FLYC_RUNNER_DIR="$DATA_DIR/runner" \
    -e FLYC_DEPLOY_KEY="$DATA_DIR/deploy/id_ed25519" \
    "$IMAGE" once -i "$INV" edge/site.yml
}
# L'installateur tourne dans un conteneur du démon qu'il configure : en posant daemon.json, le rôle
# docker fait redémarrer le démon, qui tue ce conteneur. Cela n'arrive qu'une fois — la
# configuration qu'on vient de poser active live-restore, qui garde les conteneurs en vie aux
# redémarrages suivants. Ansible étant rejouable, la reprise continue là où elle s'est arrêtée.
code=0
jouer || code=$?
case "$code" in
  0) ;;
  125|137|143)
    info "le démon Docker a redémarré et interrompu l'installation (attendu au premier passage), reprise"
    sleep 5
    jouer || die "l'installation a échoué : corriger puis relancer la même commande (elle est rejouable)"
    ;;
  *) die "l'installation a échoué : corriger puis relancer la même commande (elle est rejouable)";;
esac

step "Prêt"
info "interface    : $PUBLIC_URL"
if [ -f "$DATA_DIR/control/initial-admin-password.txt" ]; then
  echo
  sed 's/^/  /' "$DATA_DIR/control/initial-admin-password.txt"
fi
if [ -f "$DATA_DIR/control/initial-platform-api-key.txt" ]; then
  echo
  sed 's/^/  /' "$DATA_DIR/control/initial-platform-api-key.txt"
fi
cat <<EOF

  Suite depuis l'interface, Plateforme → Infrastructure :
    1. « Ajouter un nœud » donne une ligne à coller sur chaque machine à enrôler ;
    2. lui attribuer ses rôles (edge, queue), puis lancer son installation ;
    3. les réglages de plateforme (VIP, BGP, ACME) se saisissent dans la même page.
EOF
