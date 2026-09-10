# Fase A0 — Gate, toolchain e asset incorporati — **precondizione di tutto**

**Obiettivo**: avere un cancello che dice la verità. Al termine `make check` è verde, la toolchain
non ha advisory raggiungibili, e gli asset incorporati nell'immagine autoestraente sono
necessariamente coetanei del codice.

**Rilievi coperti**: A10, A11. **Decisione**: DA-05.

**Perché prima**: `Makefile:17` definisce `check: fmt vet lint build test race deps-check docs-check
proto-check`. Non contiene `selfextract`, e `embed: selfextract build` (riga 48) non è invocato dal
gate. Gli asset `internal/embedded/backimage-selfextract-linux-{amd64,arm64}` sono del 23 agosto
mentre il checkout è del 9 settembre.

**Portata reale, verificata nel ricontrollo**: CI e release **rigenerano** gli asset —
`ci.yml:26` (`make embed`, prima di `make check`) e `release.yml:53,56`. Quindi i binari
pubblicati non contengono un estrattore vecchio. Il difetto morde altrove, e comunque morde:

- chi costruisce in locale con `make build` incorpora l'estrattore che si trova sul disco, vecchio
  quanto vuole, e i test unitari girano contro quello;
- un fix di `pkg/archive` verificato in locale può risultare "funzionante" perché l'e2e locale usa
  un asset che non lo contiene, o viceversa apparire rotto senza motivo;
- la verifica manuale di un'immagine costruita in locale non dice nulla su quella costruita in CI.

Resta la prima cosa da fare, ma per la ragione giusta: **rendere impossibile che i test locali
misurino codice diverso da quello che si sta scrivendo.**

---

## A0.1 Migrazione della configurazione di lint

**Agente: Haiku**

### File: `.golangci.yml`

Lo schema attuale è della serie 1 (`run.timeout`, `linters.enable`) e abilita `gosimple`, che in
golangci-lint v2 non esiste più: è stato assorbito in `staticcheck`. L'installato in locale è
`v2.1.6`.

**Fatto scoperto nel ricontrollo, che cambia la natura del problema**: `.github/workflows/ci.yml:15`
installa `github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8`. Quindi il gate **non è
rotto in CI**: là girano configurazione v1 e binario v1, coerenti. È rotto **solo in locale**, dove
il binario è v2. Le due cose vanno cambiate nella stessa commit, altrimenti si sposta il guasto da
una parte all'altra:

- `.golangci.yml` → schema v2;
- `ci.yml:15` → `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6`.
  Il percorso del modulo in v2 contiene `/v2`: installare `@v2.1.6` dal vecchio percorso non
  funziona.

- Migrare con `golangci-lint migrate`, poi rileggere il risultato a mano: la serie 1 è EOL, non si
  torna indietro pinnando una versione vecchia.
- Verificare che l'insieme dei linter resti equivalente: `errcheck`, `govet`, `staticcheck`,
  `unused`, `ineffassign`, `misspell`, `revive`, `gosec`, `bodyclose`, `errorlint`, `contextcheck`,
  `noctx`, `copyloopvar`, `durationcheck`.
- Le direttive `//nolint:` esistenti nel codice vanno rilette: in v2 alcune motivazioni cambiano
  nome (es. `misspell` in `internal/cli/backup.go:123`).

### File: `Makefile`

```make
GOLANGCI_VERSION := v2.1.6

lint:           # G3
	@have="$$($(HOME)/go/bin/golangci-lint version --short 2>/dev/null || echo assente)"; \
	 case "$$have" in $(GOLANGCI_VERSION)*) ;; *) \
	   echo "golangci-lint $(GOLANGCI_VERSION) atteso, trovato $$have"; exit 1;; esac
	$(HOME)/go/bin/golangci-lint run
```

**Verificato**: `golangci-lint migrate` esiste in 2.1.6 ("Migrate configuration file from v1 to
v2"), e `version --short` stampa `v2.1.6` — **con** la `v` iniziale, da cui il valore della
variabile sopra. Un confronto contro `2.1.6` non matcherebbe.

**Accettazione**: `make lint` verde; con un binario di versione diversa il target fallisce con il
messaggio sopra e non con un errore di parsing della configurazione.

## A0.2 `proto-check` distingue toolchain assente da generato obsoleto

**Agente: Haiku**

### File: `scripts/check-proto.sh`

Oggi `protoc` manca in ambiente di sviluppo e il target fallisce senza distinguere le due
situazioni; in CI passa. Il risultato è che `make check` locale non è mai verde e si prende
l'abitudine di ignorarlo.

- `protoc` o `protoc-gen-go` assenti → messaggio esplicito e **exit 0** con la dicitura
  `SKIP: protoc assente, controllo non eseguito`, più una variabile `BACKIMAGE_REQUIRE_PROTOC=1`
  che rende l'assenza un errore (impostata in CI).
- Generato non aggiornato → exit non-zero, come oggi.

**Accettazione**: in locale `make check` non si ferma qui; in CI, con `BACKIMAGE_REQUIRE_PROTOC=1`,
l'assenza di `protoc` fallisce.

## A0.3 Toolchain a go1.26.6

**Agente: Sonnet**

`govulncheck ./...` riporta 15 advisory nella standard library, tutte con percorso di chiamata dal
nostro codice, tutte chiuse da `go1.26.6`:

| Advisory | Package | Fixed in |
| --- | --- | --- |
| GO-2026-6218 | `net/url` | go1.26.6 |
| GO-2026-6090, GO-2026-5856, GO-2026-4870 | `crypto/tls` | go1.26.6 / .5 / .2 |
| GO-2026-6089, GO-2026-5026, GO-2026-4918 | `net/http` | go1.26.6 / .6 / .3 |
| GO-2026-5972 | `encoding/asn1` | go1.26.6 |
| GO-2026-5039 | `net/textproto` | go1.26.4 |
| GO-2026-5037, GO-2026-4947, GO-2026-4946, GO-2026-4866 | `crypto/x509` | go1.26.4 / .2 / .2 / .2 |
| GO-2026-4971 | `net` | go1.26.3 |
| **GO-2026-4869** | **`archive/tar`** | go1.26.2 |

**Dove sta davvero il problema, verificato**: `ci.yml` e `release.yml` usano
`actions/setup-go@v5` con `go-version-file: go.mod`, e `go.mod` dichiara `go 1.26`. Il runner
risolve quella riga alla patch più recente disponibile, quindi CI e release **probabilmente già
costruiscono con una stdlib corretta**. Il buco è duplice e diverso da come lo si legge:

1. la toolchain **locale** è 1.26.1, e nessuno se ne accorge;
2. **niente pinza un minimo**: nulla vieta di costruire un rilascio con 1.26.1.

- Fissare il minimo nel modulo, che è il solo posto che vale per tutti i costruttori:
  `toolchain go1.26.6` in `go.mod`. La riga `go 1.26` resta: non alziamo il requisito di linguaggio
  per un bump di patch.
- Aggiornare la toolchain di sviluppo locale.
- `GO-2026-4869` è sul parser tar, cioè sul percorso di estrazione che la fase A2 riscrive.
  **Rieseguire l'intera batteria e2e dopo il bump, non solo i test unitari**: un parser più severo
  può rifiutare header che oggi passano. Lo stesso vale per `crypto/tls` e `crypto/x509` rispetto a
  `test/e2e/phase_08.sh` e `phase_08_stream.sh`.

### File: `Makefile`, nuovo target

```make
vuln:           # G11
	$(HOME)/go/bin/govulncheck ./...
```

Da aggiungere alla catena `check` **dopo** il bump, non prima: aggiungerlo con 15 advisory aperte
significa un gate rosso per default, che si impara a ignorare.

**Accettazione**: `make vuln` non riporta advisory raggiungibili dal nostro codice; le 7 negli
import e le 11 nei moduli richiesti, se restano non chiamate, vanno elencate nel changelog con la
ragione per cui non sono raggiungibili.

## A0.4 Gli asset incorporati entrano nella catena di build

**Agente: Sonnet**

### File: `Makefile`

```make
build: selfextract   # G4 (host) — l'immagine autoestraente non può contenere codice più vecchio
	go build -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/backimage
```

`embed` resta come alias documentato. `clean` continua a rimuovere gli asset.

### Precondizione scoperta nel ricontrollo: l'asset non ha alcuno stamp

`Makefile:43` costruisce l'autoestraente con `-ldflags '-s -w'` e **niente altro**: nessuna
iniezione di `internal/buildinfo`. In `cmd/backimage-selfextract/` non c'è alcun riferimento a
`buildinfo` né un comando `version`. Quindi **oggi il binario incorporato non dichiara nulla** e un
test di coetaneità non ha niente da confrontare. Va aggiunto lo stamp prima del test.

```make
# LDFLAGS_EMBED omette volutamente DATE: con la data il binario cambia a ogni build,
# quindi cambierebbe il digest del layer tool di ogni immagine, a codice identico.
LDFLAGS_EMBED := -s -w \
  -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
  -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT)
```

Se `cmd/backimage-selfextract` non importa `internal/buildinfo`, i flag `-X` vengono ignorati
silenziosamente: serve un riferimento reale, quindi anche un sotto-comando `version` o una riga di
help che stampi la versione. Verificare con `scripts/check-deps.sh` che l'import non trascini
dipendenze vietate (`buildinfo` è interno e non ne ha).

### File: `internal/embedded/embed_test.go`

Un test che fallisce quando l'asset non è coetaneo del codice. Non basta confrontare le date: va
confrontato ciò che l'asset dichiara.

- Confrontare lo stamp dell'asset con `buildinfo.Version` e `buildinfo.Commit` del processo
  corrente. Preferire l'estrazione dello stamp dal binario, non l'esecuzione: gli asset sono
  `linux/amd64` **e** `linux/arm64`, e su un host amd64 il secondo non è eseguibile.
- Divergenza → `t.Fatalf` con l'istruzione `make selfextract`.
- Il test non si auto-esclude in nessun caso.

**Attenzione all'effetto collaterale**: rigenerare gli asset cambia il digest del **layer tool**
delle immagini di backup. I layer dati non cambiano. Il primo backup dopo l'aggiornamento ricarica
quel layer. Verificare che nessun e2e fissi digest golden dell'immagine completa.

**Accettazione**: con asset obsoleti `make test` fallisce; `make build` li rigenera; `make check`
non può più passare con un estrattore incorporato anteriore al codice.

## A0.5 Race detector come gate dichiarato

**Agente: Haiku**

Eseguito il 10 settembre 2026 fuori sandbox: **21 package `ok`, exit 0, nessun `DATA RACE`**. Il
tentativo citato dalla review era invalido perché nel sandbox i socket locali sono vietati e la
cache di default non è scrivibile.

- Registrare in `docs/` la condizione necessaria: il gate `race` richiede socket locali e cache
  scrivibile, quindi non è eseguibile in sandbox.
- Verificare che il job CI corrispondente non sia condizionato a un `continue-on-error`.

**Accettazione**: `make race` verde in CI e in locale, senza esclusioni.

## A0.6 Changelog e nota sulle release pubblicate

**Agente: Haiku**. **Decisione**: DA-05.

### File: `CHANGELOG.md`

Nella voce 0.4.1, con la precisione che il ricontrollo impone: le release da v0.1.0 a v0.4.0 sono
state costruite **prima** di questi fix, e con una stdlib che presenta 15 advisory raggiungibili dal
codice. Gli asset incorporati di quelle release **erano** coetanei del rispettivo tag — CI e release
eseguono `make embed` — quindi non si tratta di estrattori disallineati, ma di estrattori anteriori
ai fix. Nessun ritiro degli asset. Indicare come verificare quale versione contiene una propria
immagine e come rigenerarla.

**Accettazione**: `make docs-check` verde; la nota non promette proprietà che il codice non ha.

---

## Uscita di fase

- `make check` verde in locale e in CI, `make vuln` incluso.
- Asset incorporati coetanei del codice, con test che lo impone.
- Nessun rilievo della review resta attribuibile a "gate non eseguito".
