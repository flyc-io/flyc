-- Socle multi-tenant. Les tables domaines/backends/rooms arrivent au jalon M3.
create extension if not exists pgcrypto;

create table tenants (
  id          uuid primary key default gen_random_uuid(),
  slug        text not null unique check (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  name        text not null,
  plan        text not null default 'standard',
  created_at  timestamptz not null default now()
);

create table users (
  id            uuid primary key default gen_random_uuid(),
  email         text not null,
  password_hash text,                      -- argon2id ; null si compte OIDC
  oidc_subject  text unique,
  totp_secret   bytea,                     -- chiffré avec la clé maître
  is_platform_admin boolean not null default false,
  created_at    timestamptz not null default now(),
  last_login_at timestamptz
);
create unique index users_email_idx on users (lower(email));

create table memberships (
  tenant_id  uuid not null references tenants(id) on delete cascade,
  user_id    uuid not null references users(id) on delete cascade,
  role       text not null check (role in ('owner','admin','operator','viewer')),
  primary key (tenant_id, user_id)
);

create table signing_keys (
  kid          text primary key,
  private_pem  bytea not null,   -- chiffré avec la clé maître (AES-256-GCM)
  public_pem   text not null,
  active       boolean not null default true,
  created_at   timestamptz not null default now(),
  retired_at   timestamptz
);

create table edges (
  name             text primary key,
  dataplane_url    text not null,
  version          text,
  config_hash      text,
  last_push_at     timestamptz,
  last_push_status text,
  last_seen_at     timestamptz
);

create table queue_hosts (
  name         text primary key,
  address      text not null,
  queue_port   int not null default 8080,
  redis_ports  int[] not null default '{7000,7001}',
  last_seen_at timestamptz
);

create table audit_log (
  id         bigserial primary key,
  tenant_id  uuid references tenants(id) on delete set null,
  user_id    uuid references users(id) on delete set null,
  action     text not null,
  diff       jsonb,
  at         timestamptz not null default now()
);
create index audit_log_tenant_at_idx on audit_log (tenant_id, at desc);
