-- M4 : sessions web, thème des files, mot de passe à changer, index.

create table sessions (
  id          text primary key,                 -- sha256 hex du jeton de cookie
  user_id     uuid not null references users(id) on delete cascade,
  created_at  timestamptz not null default now(),
  expires_at  timestamptz not null,
  ip          text,
  user_agent  text
);
create index sessions_user_idx on sessions(user_id);

alter table users add column if not exists must_change_password boolean not null default false;
alter table users add column if not exists display_name text not null default '';
alter table users add column if not exists totp_enabled boolean not null default false;

alter table rooms add column if not exists theme jsonb not null default '{}'::jsonb;

-- Le premier administrateur créé au bootstrap doit changer son mot de passe.
update users set must_change_password = true where email = 'admin@localhost' and last_login_at is null;

create table oidc_states (
  state       text primary key,
  nonce       text not null,
  redirect    text not null default '/',
  created_at  timestamptz not null default now()
);
