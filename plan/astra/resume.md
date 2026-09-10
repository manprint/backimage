# Stato del piano astra

**Ultimo aggiornamento**: 2026-09-10. Esecuzione in corso.

Leggere `overview.md` prima di riprendere: qui c'è solo lo stato. Le decisioni congelate sono
DA-01…DA-05 in `overview.md` §3 e non si rinegoziano senza aggiornare quel documento.

---

## Avanzamento

| Sub-fase | Rilievi | Agente | Stato |
| --- | --- | --- | --- |
| A0.1 migrazione lint a schema v2 | A10 | Haiku | **fatto** |
| A0.2 `proto-check` con esiti distinti | — | Haiku | **fatto** |
| A0.3 toolchain a go1.26.6 + target `vuln` | A10 | Sonnet | **fatto** |
| A0.4 asset incorporati nella catena di build | A11 | Sonnet | **fatto** |
| A0.5 race come gate dichiarato | — | Haiku | **fatto** |
| A0.6 changelog release pubblicate | A10, A11 | Haiku | **fatto** |
| A1.1 separazione degli opener | A01 | Opus + Sonnet | **fatto** |
| A1.2 policy `require-encryption` | A01 | Sonnet | **fatto** |
| A1.3 `--overwrite` non cancella figli estranei | A12 | Sonnet | **fatto** |
| A1.4 `--continue` non annulla i filtri | A13 | Sonnet | **fatto** |
| A2.1 traversal con `os.Root` | A03 | Opus + Sonnet | da fare |
| A2.2 hardlink dentro la radice | A02 | Sonnet | da fare |
| A2.3 backslash nei nomi Unix | A17 | Sonnet | da fare |
| A2.4 riscrittura Windows | A18 | Sonnet | da fare |
| A2.5 matrice CI oltre Linux | — | Haiku | da fare |
| A3.1 verifica prima dell'emissione | A04 | Sonnet | da fare |
| A3.2 parziale senza buffer per entry | A14 | Sonnet | da fare |
| A3.3 seek invece di letture quadratiche | A15 | Sonnet | da fare |
| A3.4 layer materializzato una volta | A16 | Sonnet | da fare |
| A4.1 scope derivato localmente | A06 | Sonnet | da fare |
| A4.2 bearer permanente non delegabile | A07 | Sonnet | da fare |
| A5.1 `--expect-digest` | A08 | Opus + Sonnet | da fare |
| A5.2 Docker fuori dall'autoestraente | A08 | Sonnet | da fare |
| A5.3 profilo confinato come esempio primario | A08 | Haiku | da fare |
| A5.4 e2e autoestraente confinato | A08 | Haiku | da fare |
| A6.0 congelare fixture dei formati attuali | A05, A20 | Sonnet | da fare, **prima di A6.1** |
| A6.1 epoca e politica nel materiale avvolto | A05 | Opus + Sonnet | da fare |
| A6.2 nonce su tutti i campi autenticati | A19 | Opus + Sonnet | da fare |
| A6.3 legame autenticato dei metadati | A20 | Opus + Sonnet | da fare |
| A6.4 dossier per review crittografica esterna | — | Opus | da fare |
| A7.1 allocazioni derivate da campi pubblici | A09 | Sonnet | da fare |
| A7.2 blob di metadati con tetto | A09 | Sonnet | da fare |
| A7.3 limite di memoria in decompressione | A09 | Sonnet | da fare |
| A7.4 limiti di forma dell'indice | A09 | Sonnet | da fare |
| A7.5 conteggi e risorse sul remoto | A09 | Sonnet | da fare |

## Da chiedere al team di quality

1. **Il documento completo.** `astra-analisys.md` si ferma ad A08: mancano i corpi di A09–A20, la
   sezione 8 con i 13 test `TestAstra…` e l'esito finale del race detector.
2. **Gli overlay dei test.** Le prove sono state applicate con `go test -overlay` e non stanno nel
   repo. Senza di essi, i criteri di accettazione di A3, A6 e A7 sono nostri: ricostruirli dai
   descrittori costa tempo e rischia di non coprire lo stesso caso.

## Vincoli tecnici verificati, da non riscoprire

- **`os.Root` in go1.26.1** copre `Open`, `OpenFile`, `Create`, `Mkdir`, `MkdirAll`, `Lstat`,
  `Stat`, `Readlink`, `Symlink`, `Link`, `Remove`, `RemoveAll`, `Rename`, `Chmod`, `Chown`,
  `Lchown`, `Chtimes`, `OpenRoot`. **Non** copre xattr, `Mknod`, FIFO, e non espone `Fd()`: il
  dirfd si ottiene aprendo la directory attraverso il `Root`. Dettagli e tabella delle cinque
  chiamate di sistema da convertire in `phase_A2.md` §A2.1.
- **L'autoestraente non ha stamp di versione**: `Makefile:43` usa `-ldflags '-s -w'` e
  `cmd/backimage-selfextract/` non importa `internal/buildinfo`. Va aggiunto prima di poter
  scrivere il test di coetaneità, con `VERSION` e `COMMIT` ma **senza** `DATE`, altrimenti il
  digest del layer tool cambia a ogni build. Vedi `phase_A0.md` §A0.4.
- **`golangci-lint version --short` stampa `v2.1.6`**, con la `v`. `golangci-lint migrate` esiste.
- **`zstd.WithDecoderMaxMemory` ha default 64 GiB.**
- **`PlainChunk` azzera il payload alla `Close`** (`pkg/recovery/recovery.go:396-405`): la doppia
  passata di A3.1 richiede prima un accessorio che esponga il compresso. Vedi `phase_A3.md` §A3.1.
- **`sendToken` considera `ExpiresAt` zero come token invalido** (`pkg/remote/client.go:334`):
  A4.2 deve distinguere "invalido" da "non delegabile".
- **`pkg/recovery/testdata` non esiste**: nessuna fixture dei formati rilasciati. Da qui A6.0.
- **`docs/security.md`** è in minuscolo; non esiste `docs/SECURITY.md`.

## Verifiche già eseguite, da non ripetere

- `go test -race -p 1 -count=1 ./...`: verde, 21 package, exit 0 (10 settembre 2026).
- `govulncheck ./...`: 15 advisory stdlib raggiungibili, tutte chiuse da go1.26.6; elenco in
  `phase_A0.md` §A0.3.
- Compatibilità dell'opener stretto: nessuna release ha mai prodotto blob `aeadNone` in un backup
  cifrato. Dimostrazione in `phase_A1.md` §A1.1.
- Profilo degli e2e: nessuno dei 11 script usa `--privileged` o monta `docker.sock`.

## Rischio residuo dichiarato

- **Review crittografica indipendente della modalità convergente**: non colmabile da questo piano.
  A6.4 produce il dossier; la chiusura richiede un revisore esterno.
- **Release già pubblicate** (v0.1.0 → v0.4.0): per DA-05 non vengono ritirate. Chi ha immagini
  prodotte con quelle versioni ha un estrattore incorporato anteriore ai fix.
- **Autenticazione dell'autoestraente dall'interno**: impossibile per costruzione. A5 la sostituisce
  con l'ancoraggio esterno del digest, non la risolve.

---

## Protocollo di sessione (aggiunto in esecuzione, 2026-09-10)

Questo file è **l'unico** file di stato del piano astra: posizione, unità in corso, ledger,
deviazioni. La tabella «Avanzamento» sopra è la board di progresso. Nessun altro file dichiara
uno stato.

### Unità in corso

| Campo | Valore |
| --- | --- |
| Tipo | — |
| ID | — |
| Stato | none |
| Intento | — |
| Prossima azione | A2.1 traversal con os.Root |
| Lavoro a metà | none — tree consistent |

### Ledger

| # | Tipo | ID | Intento | Gate | Commit |
| --- | --- | --- | --- | --- | --- |
| 1 | sub-fase | A0.1 | migrazione .golangci.yml a schema v2, pin GOLANGCI_VERSION nel Makefile, ci.yml su golangci-lint/v2@v2.1.6 | make lint verde (0 issues), make fmt/vet/build/docs-check verdi | uncommitted |
| 2 | sub-fase | A0.2 | proto-check con tre esiti distinti, risoluzione di protoc-gen-go anche da GOPATH/bin, BACKIMAGE_REQUIRE_PROTOC=1 sullo step di gate in CI | SKIP exit 0 senza toolchain; exit 1 con BACKIMAGE_REQUIRE_PROTOC=1; drift reale rilevato con protoc 27.3 + protoc-gen-go v1.34.2 (exit != 0); nessun drift sul generato corrente | uncommitted |
| 3 | sub-fase | A0.5 | condizioni d'ambiente del gate race documentate in docs/CONTRIBUTING.md; verificata l'assenza di continue-on-error in CI | make race verde in locale su go1.26.6 (21 package ok, exit 0); nessun continue-on-error né if: condizionale sul job quality in ci.yml | uncommitted |
| 4 | sub-fase | A0.4 | stamp buildinfo negli asset (LDFLAGS_EMBED senza Date), sotto-comando version nell'autoestraente, build/build-all dipendono da selfextract, test di coetaneita in internal/embedded | make lint/fmt/vet verdi; go test ./cmd/... ./internal/... verde; coeval test verificato in negativo (stamp errato e stamp assente falliscono, make selfextract ripristina); deps-check verde; nessun e2e fissa digest golden | uncommitted |
| 5 | sub-fase | A0.6 | voce 0.4.1 nel CHANGELOG: nota sulle release pubblicate, come verificare e rigenerare, elenco delle advisory non raggiungibili | make docs-check verde; make check verde; batteria e2e 11/11 verde (00 01 04 05 06 07 08 08_stream 09 10 13) | uncommitted |
| 6 | sub-fase | A1.1 | NewKeyedOpener/NewClearOpener al posto di NewOpener; ReadIndex/ReadPrivate con aspettativa esplicita; coerenza cifratura/blob privato verificata in newBackup | go test ./... verde; make lint 0 issues; docs-check verde; copertura pkg/index 69.4% -> 72.3% sui percorsi di rifiuto; TestEncryptedBackupRefusesDowngradedBlobs copre data/index/private/insieme x verify/no-verify x StreamTar/Index/StreamSelectedTar/StreamTarPartial/Verify con zero byte emessi | uncommitted |
| 7 | sub-fase | A1.2 | policy require-encryption con uscita esplicita --allow-unencrypted su entrambi gli eseguibili | go test ./... verde; make lint 0 issues; docs-check verde; docs/cli.md rigenerato; test su restore/verify/ls/find + autoestraente list/verify/tar/extract, con passphrase-file, identity ed env, piu i due casi negativi (senza credenziale, e backup cifrato con passphrase corretta) | uncommitted |
| 8 | sub-fase | A1.3 | --overwrite sovrappone invece di sostituire; RemoveAll solo su tipo discordante; O_TRUNC sulla creazione dei file regolari | go test ./... verde; make lint 0 issues; docs-check verde; TestOverwriteDoesNotDeleteChildrenTheBackupDoesNotContain verificato in negativo (con RemoveAll incondizionato fallisce); coperti figli estranei, tipi discordanti, troncamento, nomi ripetuti nel tar e il caso senza --overwrite | uncommitted |
| 9 | sub-fase | A1.4 | --continue riapplica i filtri (alreadyFiltered corretto, StreamSelectedTarPartial per l'uscita tar), e2e phase_A1.sh con l'utensile di falsificazione forgeclear, classificazione a integrita' dei rifiuti | go test ./... verde; make check verde (fmt, vet, lint 0 issues, build, test, race, deps-check, docs-check, proto-check SKIP, vuln 0 raggiungibili); make e2e PHASE=A1 verde e verificato in negativo (rimuovendo il rifiuto aeadNone lo script fallisce su 'host tar restore'); A1 aggiunta alla matrice e2e di ci.yml | uncommitted |

### Deviazioni a runtime

- A0.1 — golangci-lint v2 fonde staticcheck+gosimple+stylecheck+quickfix in un solo linter. Per non allargare il gate nella commit di migrazione, `staticcheck.checks` esclude `ST1*` e `QF1*`, che in v1 non erano attivi (nessun `stylecheck` fra i linter abilitati). Restano 9 rilievi ST1005/QF1001/QF1008 non indirizzati: abilitarli è una decisione separata con modifiche al codice.
- A0.1 — la regola `issues.exclude-rules` su `test/fixtures/compare.go` aveva una chiave `linters:` vuota che rendeva la configurazione non validabile e bloccava `golangci-lint migrate`. Rimossa la chiave vuota prima di migrare.
- A0.1 — il messaggio della guardia di versione nel Makefile è in inglese, non in italiano come nel testo di `phase_A0.md` §A0.1: AGENTS.md impone messaggi di errore in inglese.
- A0.2 — lo script non risolveva `protoc-gen-go` da `$(go env GOPATH)/bin` (guardava solo `$GOBIN`, vuoto sulla macchina di sviluppo). Aggiunta la risoluzione, altrimenti lo SKIP sarebbe scattato anche con il plugin installato.
- A0.4 — il test di coetaneita deriva l'attesa da git quando il binario di test non è marchiato (`go test` non applica LDFLAGS). `make test` non è stato modificato per iniettare gli stamp: lo farebbe rilinkare l'intero albero a ogni transizione dirty/clean. La sorgente è la stessa che usa il Makefile.
- A0.4 — `build-all` dipende da `selfextract` come `build`, non solo `build` come da piano: cross-compila lo stesso binario che incorpora `internal/embedded`, quindi soffriva dello stesso difetto.
- A0.4 — cambio di contratto: con i placeholder in posto `go test ./internal/embedded` ora **fallisce** invece di tollerare. `make check` non ne risente (`build` precede `test`), CI nemmeno (`make embed` è il primo step). Documentato in docs/BUILD.md e AGENTS.md.
- A0.3 — binfmt arm64 non era registrato sull'host: e2e phase_06 falliva con `exec /backimage: exec format error`. Registrato con `docker run --privileged --rm tonistiigi/binfmt --install arm64`, che è l'equivalente locale di `docker/setup-qemu-action` usata in CI.
- A0 uscita di fase — `make check` verde in locale (fmt, vet, lint, build, test, race, deps-check, docs-check, proto-check SKIP senza protoc, vuln 0 raggiungibili) e batteria e2e completa 11/11 dopo il bump a go1.26.6. Drift protobuf verificato a parte con protoc 27.3 reale: nessuno.
- A1.1 — l'aspettativa passa attraverso `Opener.RequiresAuthentication()` invece di un parametro aggiuntivo su ReadIndex/ReadPrivate: la politica resta legata all'oggetto che il chiamante ha scelto dal manifesto, e non si può passare un opener e un'aspettativa discordanti.
- A1.1 — copertura di pkg/index a 72.3%: il residuo è quasi tutto `format.go` (FormatLong/WriteEntries/modeString, codice di visualizzazione allo 0%), estraneo ad A01. I lettori toccati da A01 sono ReadIndex, ReadPrivate, ReadChunkTable e MergePrivate, tutti con i rami di rifiuto ora coperti.
- A1.2 — `cmdVerify` dell'autoestraente non passa da `openBackup`: apre da sé e chiama `unlock` solo se la cifratura è attiva. La policy è stata applicata anche lì, altrimenti `verify` sarebbe stato l'unico comando a benedire un backup sostituito.
- A1.2 — `info` resta fuori dalla policy su entrambi gli eseguibili: è il comando dichiaratamente senza segreti, non emette dati del backup e su un backup non cifrato non chiama nemmeno unlock.
- A1.4 — l'uscita di fase A1 ha aggiunto due cose non previste dal testo della fase. (1) test/e2e/tools/forgeclear: falsifica un backup reale riscrivendo i blob come envelope in chiaro e **riparando** ogni numero pubblico che li descrive (Sb, Ss, nome del blob, digest e storedBytes del layer, storedSha256 di index e private), poi ricostruisce l'immagine e la ripubblica su layout OCI e registry. Senza la riparazione il rifiuto sarebbe potuto arrivare da un digest discordante invece che dalla regola sull'AEAD, e il test non avrebbe provato nulla.
- A1.4 — (2) classificazione: un blob privato non autenticato faceva fallire lo sblocco e usciva 4 («passphrase errata») su entrambi gli eseguibili. Il piano chiede che il rifiuto si classifichi come integrita'/formato, quindi crypt.ErrIntegrity e index.ErrBadSchema ora mappano su exit 5 (internal/cli/errors.go, cmd/backimage-selfextract/exit.go) e lo sblocco distingue manomissione da credenziale (unlockError in entrambi). Cambio di comportamento documentato in CHANGELOG, README, README.it, docs/cron.md, docs/security.md.
- A1.4 — il daemon Docker resta fuori dalle sorgenti coperte dall'e2e: 'restore --local-repo' non legge nessun backup, nemmeno onesto, perche' docker save/daemon.Image ri-etichettano ogni layer come tar+gzip. Difetto anteriore al piano, registrato come B-A001 in plan/astra/bugs.md.

### Blocchi

_(nessuno)_

### Vicoli ciechi da non ritentare

_(nessuno)_
