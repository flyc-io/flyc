# Tests de charge

`flyc-load` joue le parcours réel d'un visiteur : ticket, flux SSE jusqu'à l'admission, `/go`,
dépôt du cookie sur le domaine protégé, page finale. Une connexion TLS HTTP/1.1 par visiteur,
comme un navigateur.

```sh
go build -o flyc-load ./loadtest/cmd/flyc-load
./flyc-load -users 20000 -ramp 60s -room demo.concert -host demo.example.com -wait https://wait.example.com
```

Options : `-resolve 203.0.113.8=wait,203.0.113.7=app` pour viser un LB précis sans DNS,
`-timeout` (attente maximale par visiteur), `-report` (période des rapports), `-path` (page finale).

Sur la machine qui génère la charge : `ulimit -n 200000` et une plage de ports suffisante
(`net.ipv4.ip_local_port_range = 10000 65535`).

Sur la plateforme, le rate limiting de prise de ticket est **par IP source** (60 par 10 s par
défaut) : pour un test depuis une seule machine, relever `haproxy_ticket_rate_limit` dans
l'inventaire, ou répartir la charge sur plusieurs sources.
