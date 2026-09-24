-- Tâches d'infrastructure exécutées par le runner pour le compte de control.
--
-- Control ne peut pas exécuter Ansible lui-même : il dépose une demande dans un répertoire
-- partagé, le runner l'exécute et y écrit le journal puis le résultat. Cette table garde la trace
-- et l'état ; le journal reste sur disque, il n'a pas sa place en base.

create table jobs (
  id          uuid primary key default gen_random_uuid(),
  kind        text not null,                       -- deploy | add-node | remove-node | sync…
  request     jsonb not null,                      -- ce qui a été demandé au runner
  status      text not null default 'pending'
              check (status in ('pending', 'running', 'succeeded', 'failed', 'interrupted')),
  exit_code   int,
  detail      text,
  created_by  text,                                -- email ou nom de clé, pour l'audit
  created_at  timestamptz not null default now(),
  started_at  timestamptz,
  finished_at timestamptz
);

create index jobs_recent on jobs (created_at desc);

-- Une seule tâche non terminée à la fois : ces opérations touchent toutes la même plateforme,
-- les laisser se chevaucher n'aurait aucun sens. La contrainte est portée par la base plutôt que
-- par le code, pour qu'elle tienne même si deux requêtes arrivent en même temps.
create unique index jobs_one_at_a_time on jobs ((true)) where status in ('pending', 'running');
