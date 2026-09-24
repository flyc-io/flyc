# SDK Node

```js
const { FlycVerifier } = require('./flyc');
const flyc = new FlycVerifier({ jwksUrl: 'https://wait.example.com/.well-known/jwks.json', host: 'billets.acme.fr', room: 'acme.concert' });

// Express : protège les routes de vente
app.use('/billets', flyc.express());
app.get('/billets', (req, res) => res.send('ticket ' + req.flyc.sub));

// ou à la main
const claims = await flyc.verify(token); // lève une erreur si invalide / expiré / autre domaine
```

Le pass arrive dans le cookie `flyc_pass` (posé par le widget) ou dans l'en-tête `X-Flyc-Pass`.
Les clés publiques sont mises en cache 5 minutes et rechargées si un `kid` est inconnu (rotation).
