# Backimage — resoconto del lavoro residuo

_Stato rilevato: 2026-08-09. Questo documento è un handoff operativo: non
spunta gate non dimostrati._

## Quadro sintetico

Le fasi 00–07 risultano completate e i relativi gate sono registrati come
superati in `plan/resume.md`. Le fasi 08–12 non sono chiuse. L'ultima
esecuzione locale di `make check` e `git diff --check` è verde; non sostituisce
i test di integrazione, le campagne privilegiate o le revisioni indipendenti
richieste dai gate.

| Fase | Stato reale | Cosa impedisce la chiusura |
|---|---|---|
| 08 | Parziale | pipeline remota ancora spool-first; manca streaming reale e gate 08 |
| 09 | Parziale | protocollo mono-stream, campagna netem completa e documento benchmark mancanti |
| 10 | Implementata e testata localmente/e2e | revisione crittografica indipendente obbligatoria |
| 11 | Parziale | adapter vendor, `repo ls`, semantica completa dei comandi, e2e e mock |
| 12 | Non avviata formalmente | documentazione finale, release, acceptance e tag |

## Fase 08 — TCP/TLS e `listen-remote`

Riferimento: `plan/phase_08.md`, sezioni 08.7 e gate.

Già presente: protocollo protobuf, framing, TLS/mTLS, sessioni, refresh token,
ACL/quote, checkpoint e test a due processi. Rimane:

1. Rifare la pipeline client di `backup --remote`: oggi produce gli strati OCI
   nello spool locale e solo dopo li invia. Il piano richiede invio mentre
   archivio/compressione/cifratura procedono, con memoria limitata e
   back-pressure.
2. Collegare in modo reale il negoziato di compressione lato server: l'avviso
   attuale non deve fingersi compressione server-side.
3. Misurare e documentare il picco di RAM e l'assenza di materiale utente sul
   disco del server durante un trasferimento grande.
4. Rieseguire l'e2e previsto dalla fase e tutti i controlli del gate 08 dopo la
   modifica. Non chiudere il gate finché il flusso non è realmente streaming.

## Fase 09 — QUIC

Riferimento: `plan/phase_09.md`, sezioni 09.3–09.5 e gate.

Già presente: trasporto QUIC, flag `--udp`, ALPN/certificati e harness
iniziale. Rimane:

1. Implementare il multiplexing previsto (stream di controllo più stream dati
   concorrenti), inclusi limiti, back-pressure e propagazione degli errori. Il
   protocollo v1 è deliberatamente mono-stream.
2. Applicare e verificare tuning di buffer/stream/GSO solo con fallback sicuri
   quando l'host o il kernel non li supportano.
3. Eseguire la matrice reale con privilegi root e `tc netem`: RTT 0/25/100/250
   ms, perdita 0/1/5 %, almeno 4 GiB per cella, TCP contro QUIC, più verifica
   di resume dopo interruzione.
4. Scrivere `docs/transport-benchmark.md` con comandi, host, versione kernel,
   risultati, limiti sperimentali e raccomandazione operativa TCP/QUIC.
5. Chiudere il gate 09 soltanto con la campagna archiviata e test ripetibili.

## Fase 10 — dedup content-defined

Riferimento: `plan/phase_10.md` e gate GS-10.3.

Sono presenti splitter Rabin, confini layer, nonce convergente/DEK stabile,
skip blob e statistiche. L'e2e reale da 4 GiB ha dimostrato il secondo upload
inferiore al 25 % e restore valido. Rimane un solo blocco formale:

1. Far eseguire a un revisore crittografico indipendente la review indicata dal
   piano: AAD in modalità convergente, derivazione/conservazione della chiave,
   collisioni, riuso keyfile, downgrade e compatibilità restore.
2. Allegare in `plan/resume.md` esito, versione/revisione esaminata, rilievi e
   loro risoluzione. Solo allora spuntare gate fase 10.

## Fase 11 — lifecycle dei repository

Riferimento: `plan/phase_11.md`. I file già aggiunti sono
`pkg/registry/adapter.go`, `adapter_oci.go`, `retention.go` e
`internal/cli/repo.go`; sono una base, non la fase completa.

### 11.1 Interfaccia e capability

1. Rendere `Capabilities` una rilevazione effettiva e cacheabile per registry,
   non solo capability dichiarate dal protocollo. I comandi devono spiegare
   chiaramente capability assente e non inviare cancellazioni di prova
   pericolose.
2. Popolare `TagInfo.Backimage` quando possibile dalle annotazioni/metadati,
   evitando download inutili; distinguere in output i tag non-backimage.
3. Aggiungere test completi di capability, errori e scelta dell'adapter per host
   con porta/suffissi vendor.

### 11.2 Adapter OCI generico

1. Verificare contro `registry:2` paginazione `Link` con 150 tag, DELETE per
   digest e 404 post-delete.
2. Conservare la protezione già impostata: se due tag puntano allo stesso
   digest, `repo rm tag` fallisce e li elenca; `--force` è l'unica eccezione.
3. Calcolare usage per blob unici senza assumere catalogo globale; documentare
   che la garbage collection non è una API OCI e quindi `CapGarbageCollect` è
   assente.

### 11.3 Adapter vendor

Mancano integralmente `adapter_ghcr.go`, `adapter_dockerhub.go`,
`adapter_ecr.go`, `adapter_quay.go` e le loro prove mockate.

1. GHCR: DELETE OCI con token `packages:delete`; usare GitHub Packages per
   lista tag quando disponibile.
2. Docker Hub: DELETE tag tramite API Hub e JWT; non dichiarare DELETE OCI.
3. ECR: decisione esplicita. Senza una implementazione SigV4 manuale auditable
   non aggiungere SDK AWS: dichiarare delete assente e rimandare alle lifecycle
   policy ECR.
4. Quay: DELETE tag tramite API Quay OAuth.
5. Per tutti: dichiarare le capability reali, non loggare token e testare
   autenticazione/errori con server mock.

### 11.4 CLI

1. Implementare `repo ls <REGISTRY>` quando l'adapter supporta catalogo.
2. Completare `repo tags` con metadati backimage, ordinamento e output
   umano/JSON stabilizzato.
3. Completare `repo prune` con hourly/daily/weekly/monthly/yearly,
   `--keep-within`, glob multipli e default sicuro: tag sconosciuti non vengono
   rimossi salvo `--all` esplicito.
4. Rendere la conferma conforme al piano: digitazione del tag esatto o `--yes`;
   in JSON solo `--yes`; dry-run senza rete distruttiva. Mantenere `--force`
   esplicito per digest condivisi.
5. Rendere `repo caps` leggibile (nomi capability, non solo bitmask) e basato
   sulla rilevazione del punto 11.1.

### 11.5 Retention

Il motore puro e i test sono presenti. Da completare: connetterlo alla semantica
dei tag backimage/non-backimage, aggiungere tutti i casi tabellari prescritti
dal piano ed eseguire property test più ampi sugli invarianti (determinismo,
nessuna rimozione di tag selezionato, partizione completa).

### 11.6 E2E e gate

Manca `test/e2e/phase_11.sh`. Deve creare 30 backup datati, verificare lista,
dry-run da 25 rimozioni, prune a 5, restore del più vecchio superstite, stats e
messaggio corretto quando DELETE è disabilitato. Aggiungere anche mock vendor.
Poi eseguire `make e2e PHASE=11`, coverage richiesta dal piano e gate 11.

## Fase 12 — documentazione e release v1.0.0

Riferimento: `plan/phase_12.md`. I seguenti artefatti richiesti non esistono:
`docs/troubleshooting.md`, `docs/FAQ.md`, `.goreleaser.yaml`,
`.github/workflows/release.yml`, `test/e2e/acceptance.sh`.

1. Rifinire README (<250 righe): quick start, tabella di tutti i comandi,
   sicurezza, registri supportati e limiti noti sinceri (streaming fase 08,
   benchmark QUIC, adapter vendor e gate crypto inclusi).
2. Completare inventario `docs/`: troubleshooting, FAQ, architettura, formati,
   sicurezza, trasporto e dedup; ogni esempio deve essere eseguibile.
3. Generare e verificare man page e completion shell per bash/zsh/fish/powershell.
4. Configurare GoReleaser e workflow: `make embed`, 8 target, CGO disabilitato,
   trimpath/ldflags, tar.gz/zip, checksum SHA-256, SBOM, cosign keyless e
   immagine GHCR multi-arch distroless/scratch.
5. Aggiornare `CHANGELOG.md` con `v1.0.0` e provare prima un tag rc con
   `id-token: write`.
6. Creare acceptance: backup cifrato locale, restore, inspect/verify, remote,
   self-extract da binario di release e controlli di qualità.
7. Eseguire tutti i gate: cover complessiva >=80 %, e2e 01–11, docs-check,
   help di ogni comando, zero TODO/FIXME/XXX non tracciati, verifica cosign e
   review Opus finale. Solo con esito verde creare il tag `v1.0.0`.

## Ordine consigliato

1. Finire 08 e 09: sono fondazioni che cambiano protocollo e documentazione.
2. Ottenere la review della fase 10 prima di ampliare l'uso della dedup.
3. Chiudere fase 11 con e2e `registry:2`, poi adapter vendor mockati.
4. Completare fase 12 e fare un release candidate; non pubblicare `v1.0.0`
   finché tutti i gate elencati sopra non sono dimostrati.
