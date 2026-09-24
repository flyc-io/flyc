-- M5 : mode automatique (seuils de connexions sur le backend), état effectif.
alter table rooms drop constraint if exists rooms_state_check;
alter table rooms add constraint rooms_state_check check (state in ('open','queue','closed','auto'));
alter table rooms add column if not exists auto_on  int not null default 0;   -- connexions backend au-dessus desquelles la file s'active
alter table rooms add column if not exists auto_off int not null default 0;   -- connexions en dessous desquelles elle se désactive
alter table rooms add column if not exists effective_state text not null default 'open' check (effective_state in ('open','queue','closed'));
update rooms set effective_state = state where state in ('open','queue','closed');
