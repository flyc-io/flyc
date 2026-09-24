# SDK PHP

```php
require 'Flyc.php';
$flyc = new FlycVerifier('https://wait.example.com/.well-known/jwks.json', 'billets.acme.fr', 'acme.concert');
try {
    $claims = $flyc->verify(FlycVerifier::tokenFromRequest());   // cookie flyc_pass ou en-tête X-Flyc-Pass
} catch (RuntimeException $e) {
    http_response_code(403); exit('accès réservé aux visiteurs admis : ' . $e->getMessage());
}
```

Les clés publiques sont mises en cache (fichier temporaire, 5 minutes) et rechargées si un `kid`
est inconnu.
