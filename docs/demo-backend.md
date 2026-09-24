# Backend de démonstration

Les backends (les serveurs qui rendent le site du client) ne font pas partie
du déploiement Flyc. Pour l'environnement de test, on en monte un à la main
sur n'importe quelle machine joignable depuis les LB, exactement comme le
ferait un vrai client.

## Fausse billetterie statique (nginx)

```sh
mkdir -p /srv/demo-shop
cat > /srv/demo-shop/index.html <<'HTML'
<!doctype html><html lang="fr"><meta charset="utf-8"><title>Billetterie démo</title>
<body style="font-family:system-ui;max-width:40rem;margin:10vh auto">
<h1>Billetterie démo</h1>
<p>Si vous voyez cette page, vous avez traversé la salle d'attente avec un pass valide.</p>
<p>Room : <code id="r"></code> · Pass : <code id="s"></code></p>
</body></html>
HTML
docker run -d --name demo-shop --restart unless-stopped -p 8081:80 \
  -v /srv/demo-shop:/usr/share/nginx/html:ro nginx:alpine
```

Le backend écoute sur `<ip>:8081`. Il est ensuite déclaré dans l'interface
flyc-control (ou, en attendant M3, ajouté dans `backends.map` et dans un
backend HAProxy via la Dataplane API).

HAProxy ajoute `X-Flyc-Room` et `X-Flyc-Pass-Sub` aux requêtes admises : une
vraie application peut s'en servir pour journaliser ou limiter la durée de
session.
