-- M3 : domaines, backends, files, certificats, clés API, ACME.

create table api_keys (
  id          uuid primary key default gen_random_uuid(),
  tenant_id   uuid references tenants(id) on delete cascade,   -- null = clé plateforme
  name        text not null,
  key_hash    text not null unique,                             -- sha256 hex
  created_at  timestamptz not null default now(),
  last_used_at timestamptz
);

create table domains (
  id          uuid primary key default gen_random_uuid(),
  tenant_id   uuid not null references tenants(id) on delete cascade,
  fqdn        text not null unique check (fqdn = lower(fqdn)),
  mode        text not null default 'dns' check (mode in ('dns','iframe')),
  tls         text not null default 'auto' check (tls in ('auto','none')),
  created_at  timestamptz not null default now()
);
create index domains_tenant_idx on domains(tenant_id);

create table backends (
  id          uuid primary key default gen_random_uuid(),
  tenant_id   uuid not null references tenants(id) on delete cascade,
  name        text not null check (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  balance     text not null default 'roundrobin' check (balance in ('roundrobin','leastconn','source')),
  check_path  text not null default '/',
  created_at  timestamptz not null default now(),
  unique (tenant_id, name)
);

create table backend_servers (
  id          uuid primary key default gen_random_uuid(),
  backend_id  uuid not null references backends(id) on delete cascade,
  name        text not null check (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  address     text not null,
  port        int  not null check (port between 1 and 65535),
  weight      int  not null default 100 check (weight between 0 and 256),
  maxconn     int  not null default 0,
  unique (backend_id, name)
);

create table rooms (
  id          uuid primary key default gen_random_uuid(),
  tenant_id   uuid not null references tenants(id) on delete cascade,
  slug        text not null check (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  title       text not null default '',
  domain_id   uuid references domains(id) on delete set null,
  backend_id  uuid references backends(id) on delete set null,
  state       text not null default 'open' check (state in ('open','queue','closed')),
  rate        double precision not null default 10,
  max_active  bigint not null default 0,
  wave_s      int not null default 30,
  pass_ttl_s  int not null default 1200,
  opens_at    timestamptz,
  updated_at  timestamptz not null default now(),
  created_at  timestamptz not null default now(),
  unique (tenant_id, slug)
);
create unique index rooms_domain_idx on rooms(domain_id) where domain_id is not null;

create table certificates (
  id          uuid primary key default gen_random_uuid(),
  domain_id   uuid not null unique references domains(id) on delete cascade,
  fqdn        text not null,
  issuer      text,
  not_before  timestamptz,
  not_after   timestamptz,
  pem_enc     bytea,                      -- fullchain + clé, chiffré avec la clé maître
  status      text not null default 'pending',   -- pending | issued | error
  last_error  text,
  last_attempt_at timestamptz,
  deployed_hash text,                     -- sha256 du PEM déployé
  updated_at  timestamptz not null default now()
);

create table acme_accounts (
  directory   text primary key,
  email       text not null,
  key_enc     bytea not null,             -- clé privée du compte, chiffrée
  registration jsonb,
  created_at  timestamptz not null default now()
);

create table acme_challenges (
  token       text primary key,
  key_auth    text not null,
  fqdn        text not null,
  created_at  timestamptz not null default now()
);

alter table edges add column if not exists desired_hash text;
alter table edges add column if not exists last_error text;
alter table queue_hosts add column if not exists last_error text;

-- Version de la configuration désirée : incrémentée à chaque changement, lue par le réconciliateur.
create table config_version (
  id      int primary key default 1 check (id = 1),
  version bigint not null default 1,
  updated_at timestamptz not null default now()
);
insert into config_version(id) values (1) on conflict do nothing;

create or replace function bump_config_version() returns trigger language plpgsql as $$
begin
  update config_version set version = version + 1, updated_at = now() where id = 1;
  return null;
end $$;
create trigger domains_bump after insert or update or delete on domains for each statement execute function bump_config_version();
create trigger backends_bump after insert or update or delete on backends for each statement execute function bump_config_version();
create trigger backend_servers_bump after insert or update or delete on backend_servers for each statement execute function bump_config_version();
create trigger rooms_bump after insert or update or delete on rooms for each statement execute function bump_config_version();
create trigger certificates_bump after insert or update or delete on certificates for each statement execute function bump_config_version();
