-- Enrôlement d'un nœud : une machine vierge se déclare auprès de control avec un jeton à usage
-- unique, et repart avec le compte de déploiement et la clé publique. Rien de secret ne circule —
-- le script ne transporte qu'une clé publique ; le jeton sert à lier la machine à une demande et à
-- empêcher un inconnu de s'inscrire.
create table enroll_tokens (
  id          uuid primary key default gen_random_uuid(),
  -- Le jeton n'est pas stocké en clair : il voyage dans une URL et ne sert qu'une fois. Une lecture
  -- de la base ne permet donc pas de rejouer un enrôlement.
  token_hash  text not null unique,
  roles       text[] not null default '{}',   -- rôles prévus, indicatifs tant que le nœud n'est pas créé
  created_by  text not null default '',
  created_at  timestamptz not null default now(),
  expires_at  timestamptz not null,
  used_at     timestamptz,
  hostname    text not null default '',
  mgmt_ip     text not null default '',
  mgmt_ip6    text not null default ''
);

create index enroll_tokens_expires on enroll_tokens(expires_at);
