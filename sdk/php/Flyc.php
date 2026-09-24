<?php
// SDK Flyc pour PHP — vérification des pass (JWT ES256) avec les clés publiques de la plateforme.
// PHP ≥ 8.0, extension openssl. Aucune dépendance.
final class FlycVerifier
{
    private array $keys = [];
    private int $fetchedAt = 0;

    public function __construct(private string $jwksUrl, private string $host, private ?string $room = null, private int $cacheSeconds = 300, private ?string $cacheFile = null)
    {
        $this->host = strtolower($host);
        $this->cacheFile ??= sys_get_temp_dir() . '/flyc-jwks-' . md5($jwksUrl) . '.json';
    }

    /** Vérifie un pass et renvoie ses claims ; lève une RuntimeException sinon. */
    public function verify(string $token): array
    {
        $parts = explode('.', $token);
        if (count($parts) !== 3) throw new RuntimeException('format invalide');
        [$h, $p, $s] = $parts;
        $header = json_decode(self::b64($h), true);
        if (($header['alg'] ?? '') !== 'ES256') throw new RuntimeException('algorithme inattendu');
        $this->refresh(false);
        $pem = $this->keys[$header['kid'] ?? ''] ?? null;
        if ($pem === null) { $this->refresh(true); $pem = $this->keys[$header['kid'] ?? ''] ?? null; }
        if ($pem === null) throw new RuntimeException('clé inconnue : ' . ($header['kid'] ?? '?'));
        $sig = self::b64($s);
        if (strlen($sig) !== 64) throw new RuntimeException('signature invalide');
        $der = self::joseToDer(substr($sig, 0, 32), substr($sig, 32));
        if (openssl_verify($h . '.' . $p, $der, $pem, OPENSSL_ALGO_SHA256) !== 1) throw new RuntimeException('signature invalide');
        $claims = json_decode(self::b64($p), true) ?: [];
        if (($claims['exp'] ?? 0) <= time()) throw new RuntimeException('pass expiré');
        if (strtolower($claims['dom'] ?? '') !== $this->host) throw new RuntimeException('pass pour un autre domaine');
        if ($this->room !== null && ($claims['rm'] ?? '') !== $this->room) throw new RuntimeException('pass pour une autre file');
        return $claims;
    }

    /** Pass présenté par la requête courante : cookie flyc_pass ou en-tête X-Flyc-Pass. */
    public static function tokenFromRequest(): string
    {
        return $_SERVER['HTTP_X_FLYC_PASS'] ?? $_COOKIE['flyc_pass'] ?? '';
    }

    private function refresh(bool $force): void
    {
        if (!$force && $this->keys && time() - $this->fetchedAt < $this->cacheSeconds) return;
        if (!$force && $this->cacheFile && is_file($this->cacheFile) && time() - filemtime($this->cacheFile) < $this->cacheSeconds) {
            $this->load((string) file_get_contents($this->cacheFile)); return;
        }
        $json = @file_get_contents($this->jwksUrl, false, stream_context_create(['http' => ['timeout' => 5]]));
        if ($json === false) throw new RuntimeException('JWKS injoignable');
        if ($this->cacheFile) @file_put_contents($this->cacheFile, $json);
        $this->load($json);
    }

    private function load(string $json): void
    {
        $this->keys = [];
        foreach ((json_decode($json, true)['keys'] ?? []) as $jwk) {
            if (($jwk['kty'] ?? '') === 'EC' && ($jwk['crv'] ?? '') === 'P-256') $this->keys[$jwk['kid']] = self::jwkToPem($jwk['x'], $jwk['y']);
        }
        $this->fetchedAt = time();
    }

    private static function b64(string $s): string { return (string) base64_decode(strtr($s, '-_', '+/') . str_repeat('=', (4 - strlen($s) % 4) % 4)); }

    /** SubjectPublicKeyInfo P-256 à partir des coordonnées JWK. */
    private static function jwkToPem(string $x, string $y): string
    {
        $prefix = hex2bin('3059301306072a8648ce3d020106082a8648ce3d030107034200'); // SEQ { SEQ { ecPublicKey, prime256v1 }, BITSTRING }
        $der = $prefix . "\x04" . self::b64($x) . self::b64($y);
        return "-----BEGIN PUBLIC KEY-----\n" . chunk_split(base64_encode($der), 64, "\n") . "-----END PUBLIC KEY-----\n";
    }

    private static function joseToDer(string $r, string $s): string
    {
        $int = static function (string $v): string { $v = ltrim($v, "\x00"); if ($v === '' || ord($v[0]) > 0x7f) $v = "\x00" . $v; return "\x02" . chr(strlen($v)) . $v; };
        $body = $int($r) . $int($s);
        return "\x30" . chr(strlen($body)) . $body;
    }
}
