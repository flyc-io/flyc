-- Les images sont désormais publiées sur un registre public : c'est ce qu'une nouvelle
-- installation doit trouver par défaut. Les plateformes existantes gardent leur valeur.
alter table platform_settings alter column registry set default 'ghcr.io/flyc-io';
