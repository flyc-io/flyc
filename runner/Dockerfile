# Image d'exécution : elle embarque les playbooks et sait joindre les nœuds en SSH.
#
# flyc-control ne peut pas faire ce travail : c'est une Alpine avec le seul binaire Go, sans Python
# ni SSH, et surtout elle ne doit jamais voir la clé privée de déploiement. Control décide, ce
# conteneur exécute ; ils n'échangent que par un répertoire partagé.
#
# Les playbooks sont dans l'image, pas clonés au démarrage : leur version suit celle du binaire,
# sans identifiants git ni accès réseau au dépôt.
# Base Python plutôt que Debian nu : ansible-core 2.20 exige Python 3.12, que Debian 12 ne fournit
# pas. C'est aussi l'image que la CI utilise déjà pour ses étapes de test.
FROM python:3.12-slim

ARG VERSION=dev
ARG ANSIBLE_CORE=2.20.3
LABEL org.opencontainers.image.title="flyc-runner" org.opencontainers.image.version="${VERSION}"

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      openssh-client ca-certificates curl rsync \
 && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir "ansible-core==${ANSIBLE_CORE}"

# ansible.cfg déclare inventory et roles_path en relatif : le répertoire de travail doit être la
# racine du dépôt, sinon les rôles ne sont pas trouvés.
WORKDIR /opt/flyc
COPY ansible.cfg ./
COPY edge/ edge/
COPY tools/ tools/
COPY deploy/ deploy/
COPY wait/dist/ wait/dist/

ENV ANSIBLE_COLLECTIONS_PATH=/opt/ansible/collections
RUN ansible-galaxy collection install -r edge/requirements.yml -p "$ANSIBLE_COLLECTIONS_PATH"

COPY runner/run.py /usr/local/bin/flyc-runner
RUN chmod 0755 /usr/local/bin/flyc-runner

# Le conteneur tourne en root : il lit la clé de déploiement (root:root 0700) et écrit dans le
# répertoire d'échange. Il n'expose aucun port et ne reçoit aucune connexion.
ENTRYPOINT ["/usr/local/bin/flyc-runner"]
