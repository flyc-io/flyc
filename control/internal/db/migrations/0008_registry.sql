-- Le registre d'images était codé en dur dans l'inventaire généré : une plateforme installée
-- ailleurs qu'ici ne pouvait pas en changer depuis l'interface.
alter table platform_settings add column registry text not null default 'registry.example.com/flyc';
