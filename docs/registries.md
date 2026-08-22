# Registry OCI

`backimage` usa il protocollo Registry HTTP API V2: discovery su `/v2/`,
token bearer con refresh proattivo, upload `POST` → `PATCH` a chunk → `PUT`,
manifest OCI per piattaforma e image index multi-architettura.

Per la guida utente end-to-end, con esempi di Docker Hub, login multipli e
restore, vedere [`README.md`](../README.md) per la sintesi e
[handbook.it.md](handbook.it.md#autenticazione-dei-registry) per le ricette
complete. Questa pagina raccoglie i dettagli tecnici del comportamento.

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

Più account sullo stesso registry canonico convivono. Il primo occupa la chiave
host, compatibile con Docker e con le versioni precedenti di backimage; ogni
account successivo usa una chiave `host#username` (`pkg/registry/auth.go`), e un
bearer token salvato con `--token` ha una propria identità riservata, mostrata
come `token`. Nessun login sovrascrive gli altri.

La selezione avviene per **namespace del repository**: `docker.io/user2/img` usa
il login `user2`. Se il namespace non corrisponde ad alcun account salvato il
comando si ferma invece di pubblicare con l'identità sbagliata, e `--registry-user
NOME` (flag globale) forza la scelta; `--registry-user none` forza una richiesta
anonima.

`backimage login --list` elenca provider, account e utente locale proprietario
del file (`--json` aggiunge `authFile`), mai i segreti. `backimage logout
REGISTRY` rimuove l'unico account; con più account servono `--user NOME` oppure
`--all`, altrimenti il comando si ferma elencandoli. Il logout riguarda il
registry, non il singolo repository.

File separati via `BACKIMAGE_AUTH_FILE` restano utili per isolare del tutto i
contesti, per esempio in CI, ma non sono più necessari per avere più account.
Ricette complete in [handbook.it.md](handbook.it.md#login-multipli-più-account-sullo-stesso-registry).

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

## Cancellazione e manifest condivisi

L'API OCI cancella **per digest**: `DELETE /v2/<name>/manifests/<digest>`. Un
tag non è un oggetto separato che si possa rimuovere da solo, quindi eliminare
un manifest fa sparire **tutti** i tag che lo referenziano.

Questo ha una conseguenza concreta sui backup: due dump identici di sorgenti
diverse producono la stessa immagine e quindi lo stesso manifest. Se `db_1` e
`app_1` condividono il manifest, cancellare `db_1` cancellerebbe anche `app_1`.

`repo prune` verifica per questo l'intero piano prima della prima richiesta:

- se un manifest da eliminare è ancora referenziato da un tag che la policy
  conserva, il comando **rifiuta ed elenca i tag coinvolti**, senza inviare
  nessuna DELETE. Non esiste uno stato intermedio in cui una parte dei tag è
  già stata cancellata e il comando è uscito in errore;
- se invece tutti i tag di un manifest sono nell'insieme da eliminare, il
  manifest viene rimosso con **una sola** richiesta, e non serve `--force`.

`repo rm --force` resta la via per eliminare deliberatamente insieme i tag che
condividono un manifest.

Due note operative:

- la cancellazione va abilitata sul registry. `registry:2` la rifiuta se non si
  imposta `REGISTRY_STORAGE_DELETE_ENABLED=true`; il messaggio d'errore di
  backimage lo ricorda;
- eliminare il manifest non libera subito lo spazio dei blob. Il recupero
  dipende dal garbage collector del registry e va eseguito a parte
  (`registry garbage-collect` su `registry:2`). Nessun adapter di backimage
  espone oggi quell'operazione: `repo caps` dichiara `CapListTags`,
  `CapDeleteManifest`, `CapDeleteTag` e `CapUsageStats`, non `CapGarbageCollect`.

### Un limite che resta

Il pre-check elimina la causa *prevedibile* di un'interruzione a metà, ma una
sequenza di DELETE HTTP non può essere atomica: un errore di rete sul terzo di
cinque manifest lascia comunque i primi due eliminati. `prune` in quel caso dice
fino a dove è arrivato — `2 manifest su 5 erano già stati eliminati` — e
rieseguire lo stesso comando completa il lavoro, perché la policy è idempotente:
i manifest già rimossi non compaiono più nel piano.
