# Registry OCI

`backimage` usa il protocollo Registry HTTP API V2: discovery su `/v2/`,
token bearer con refresh proattivo, upload `POST` → `PATCH` a chunk → `PUT`,
manifest OCI per piattaforma e image index multi-architettura.

Per la guida utente end-to-end, con esempi di Docker Hub, login multipli e
restore, vedere la sezione [Autenticazione dei registry](../README.md#autenticazione-dei-registry)
nel README. Questa pagina raccoglie i dettagli tecnici del comportamento.

## Autenticazione

```sh
printf '%s\n' "$REGISTRY_PASSWORD" | \
  backimage login REGISTRY --username USER --password-stdin
backimage login --list --json
backimage logout REGISTRY
```

Le credenziali sono risolte in ordine:

1. credenziali esplicite;
2. `BACKIMAGE_AUTH_FILE`, se impostata, oppure
   `$XDG_CONFIG_HOME/backimage/auth.json`;
3. `$HOME/.config/backimage/auth.json` quando `XDG_CONFIG_HOME` non è impostata;
4. configurazione Docker e credential helper;
5. accesso anonimo.

`auth.json` usa il formato Docker `auths`, viene scritto atomicamente con
permessi 0600 e viene rifiutato se leggibile da gruppo o altri. I tre alias
`docker.io`, `index.docker.io` e `registry-1.docker.io` sono equivalenti.
`--token` salva un bearer token già pronto; dopo un 401 la richiesta viene
ritentata una sola volta.

Lo store contiene una sola credenziale per registry canonico: un nuovo login
allo stesso registry sostituisce il precedente. `backimage login --list` mostra
solo i registry configurati (`--json` produce JSON); `backimage logout REGISTRY`
rimuove l'intero login del registry. Il logout non è disponibile a livello di
repository: per usare account diversi sullo stesso registry occorre usare file
separati tramite `BACKIMAGE_AUTH_FILE`.

## Compatibilità

| Registry | Login/reference | Manifest list OCI | Note |
|---|---|---:|---|
| Docker Hub | `index.docker.io/utente/repo` | sì | token brevi: refresh automatico |
| GHCR | `ghcr.io/org/repo` | sì | usare PAT con scope packages |
| Quay | `quay.io/org/repo` | sì | verificare quota e robot account |
| Amazon ECR | `ACCOUNT.dkr.ecr.REGION.amazonaws.com/repo` | sì | password ottenuta da AWS CLI/credential helper |
| Harbor | `harbor.example/org/repo` | sì | supportati auth basic e bearer |
| `registry:2` | `localhost:PORT/repo` | sì | test e2e ufficiale del progetto |

I limiti massimi dei blob dipendono dalla configurazione del servizio e
possono cambiare. Tenere `--max-layer-size` sotto il limite documentato dal
proprio registry; valori tra 512 MiB e 4 GiB sono in genere più interoperabili.
Alcune installazioni ECR/Harbor datate rifiutano annotazioni OCI sconosciute:
in quel caso aggiornare il registry o usare un repository dedicato e verificare
prima con un backup piccolo.

## Affidabilità del push

- `HEAD` evita upload già presenti;
- config e layer sono deduplicati fra piattaforme;
- 5xx e 429 sono ritentati con backoff e `Retry-After`;
- 403 non viene ritentato;
- ogni `PATCH` adotta il nuovo `Location` restituito dal registry;
- i manifest di piattaforma vengono pubblicati prima dell'image index;
- un checkpoint corrotto o riferito a un'altra reference blocca il resume.

Nessun log include password, token o contenuto di `auth.json`.
