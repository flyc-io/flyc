-- L'inventaire devient une donnée de la plateforme, plus un fichier sur le poste d'un opérateur.
-- Control génère le YAML depuis ces tables et le joint à chaque tâche ; le runner ne fait
-- qu'exécuter. Les tables edges et queue_hosts restent ce qu'elles sont — des registres d'état
-- (in_sync, last_push_status) — et sont désormais alimentées depuis nodes.

create table nodes (
  name        text primary key,
  -- Plusieurs rôles par machine : c'est exactement le profil all-in-one, où un seul hôte porte
  -- les LB, la file et la base. Une ligne par machine, pas par rôle.
  roles       text[] not null check (roles <@ array['edge','queue','data'] and array_length(roles, 1) >= 1),
  mgmt_ip     text not null,
  mgmt_ip6    text not null default '',
  -- planned : déclaré, pas encore installé. ready : en service. draining : en cours de retrait.
  state       text not null default 'planned' check (state in ('planned','installing','ready','draining')),
  note        text not null default '',
  enrolled_at timestamptz,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now()
);

-- Deux nœuds ne peuvent pas partager une adresse : l'inventaire généré serait ambigu.
create unique index nodes_mgmt_ip on nodes(mgmt_ip);

-- Une seule ligne : les variables communes à toute la plateforme (le « all.vars » de l'inventaire).
create table platform_settings (
  id                 int primary key default 1 check (id = 1),
  env                text not null default 'prod',
  ansible_user       text not null default 'flyc-deploy',
  wait_host          text not null default '',
  control_host       text not null default '',
  vip_app_v4         text not null default '',
  vip_app_v6         text not null default '',
  vip_wait_v4        text not null default '',
  vip_wait_v6        text not null default '',
  vip_gateway_v4     text not null default '',
  vip_gateway_v6     text not null default '',
  bgp_enabled        boolean not null default true,
  bgp_local_as       int not null default 0,
  -- [{ip, remote_as, family}] : la forme attendue par le rôle bird.
  bgp_neighbors      jsonb not null default '[]',
  acme_email         text not null default '',
  acme_directory     text not null default '',
  control_public_url text not null default '',
  cookie_secure      boolean not null default true,
  image_tag          text not null default 'latest',
  queue_port         int not null default 8080,
  control_port       int not null default 8090,
  dataplane_port     int not null default 5555,
  manage_firewall    boolean not null default true,
  deploy_from        text not null default '',
  -- Certificat racine du registre d'images, en PEM. Téléversé une fois depuis l'interface :
  -- un chemin de fichier n'aurait aucun sens ici, control et le runner ne partagent pas le poste
  -- de l'opérateur qui l'avait à l'origine.
  registry_ca        text not null default '',
  -- Réglages libres recopiés tels quels dans all.vars (ex. haproxy_ticket_rate_limit).
  extra_vars         jsonb not null default '{}',
  seeded_from_env    boolean not null default false,
  updated_at         timestamptz not null default now()
);

insert into platform_settings(id) values (1);
