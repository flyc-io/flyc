# Contribuer

Les contributions sont bienvenues : correctifs, rôles Ansible, pilotes, documentation.

## Avant d'ouvrir une pull request

Pour un changement important — un nouveau rôle, une modification du schéma, un changement de
comportement à l'exploitation — ouvrez d'abord une issue. Il est plus rapide de se mettre d'accord
sur l'approche que de refaire un travail déjà écrit.

## Ce qui doit passer

```sh
go vet ./... && go test ./...
ansible-playbook -i tools/fixtures/inventory.yml edge/site.yml --syntax-check
for f in tools/*.sh; do bash -n "$f"; done
```

La CI rejoue tout cela, plus `yamllint` et la validation de la configuration HAProxy rendue — un
template qui ne tient pas debout est refusé avant d'atteindre un load balancer.

Deux exigences propres au projet, parce qu'elles coûtent cher quand on les oublie :

* **Idempotence.** Une passe d'`edge/site.yml` sans changement doit finir à `0 changed`. Une tâche
  qui se déclare modifiée à chaque exécution masque les vraies modifications.
* **Un retrait doit être vérifié, pas supposé.** Les playbooks de mise hors service finissent par
  un contrôle réel (plus aucun port Flyc à l'écoute). Si vous ajoutez un service, ajoutez-le à ce
  contrôle.

## Style

Le code et les commentaires sont en français. Les commentaires expliquent *pourquoi*, pas *quoi* :
la valeur d'un commentaire est ce qu'il apprend à celui qui devra modifier le code dans six mois.

Messages de commit : une ligne de résumé à l'impératif, puis ce que le changement corrige et
pourquoi cette approche. Les pièges rencontrés en chemin ont leur place dans le message.

## Licence

Le projet est publié sous AGPL-3.0 (voir `LICENSE`). En proposant une modification, vous acceptez
qu'elle soit distribuée sous cette licence.
