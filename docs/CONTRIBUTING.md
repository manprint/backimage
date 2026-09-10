# Contribuire

> «Chi implementa NON deve prendere decisioni architetturali: sono tutte già
> prese e scritte nel piano (`plan/`).»

## Le dieci regole ferree

1. **Implementa solo i file elencati** nella sotto-fase corrente. Nessun altro file va creato o modificato.
2. **Non inventare API.** Le firme esportate sono scritte nel file di fase: copiale alla lettera, incluso l'ordine dei parametri.
3. **Non aggiungere dipendenze.** Se sembra servirne una, non si sta procedendo nel modo giusto: fermati e segnala.
4. **Non modificare un test per farlo passare.** Se un test sembra sbagliato, fermati e segnala.
5. **Non rifattorizzare** codice fuori dalla sotto-fase corrente.
6. **Non saltare sotto-fasi.** L'ordine è vincolante.
7. **Un commit per sotto-fase**, messaggio `feat(NN.x): <titolo>` (o `test:`/`docs:`/`chore:` quando appropriato).
8. **Errori sempre avvolti** con `fmt.Errorf("contesto: %w", err)`. Mai `panic` fuori da `main`. Mai ignorare un `err` con `_`.
9. **Niente stato globale mutabile**, niente `init()` con effetti collaterali (le mappe di registrazione dei plugin sono l'unica eccezione).
10. **Ogni funzione che fa I/O accetta `context.Context` come primo parametro.**

## Loop di autocorrezione

1. Leggi l'intera sotto-fase, inclusa la sezione "Definition of Done".
2. Scrivi i test PRIMA dell'implementazione, quando la sotto-fase li specifica.
3. Implementa.
4. Esegui: `make check`.
5. Se fallisce: leggi SOLO il primo errore, correggi SOLO quello, ripeti.
6. Massimo 5 iterazioni sullo stesso errore; oltre, scrivi `plan/BLOCKED.md` e fermati.

## Convenzioni

- Go 1.26, `CGO_ENABLED=0` sempre.
- Nomi identificatori, commenti e messaggi di errore **in inglese**. Documentazione utente in italiano.
- Messaggi di errore: minuscoli, senza punto finale, con contesto.
- Ogni pacchetto ha un `doc.go` di 5–15 righe.
- Nessun output su `stdout` che non sia il dato richiesto: log e progresso su `stderr`.
- Codici di uscita: 0 ok, 1 generico, 2 uso, 3 privilegi, 4 passphrase, 5 integrità, 6 rete, 7 interrotto.

## Fixture dei formati rilasciati

`pkg/recovery/testdata/` contiene un backup completo per ogni formato su disco
che questo progetto ha rilasciato: `schema1-plain`, `schema2-encrypted` e
`legacy-envelope1` (envelope v1, scritto fino alla 0.2.3 e da allora solo
letto). Non sono generate a runtime: appena il writer cambia, nessuna build è
più in grado di produrre i byte vecchi, e un test di compatibilità che genera
il proprio input smette silenziosamente di coprire qualcosa.

`pkg/recovery/format_compat_test.go` le apre, le elenca, le ripristina e le
verifica. **Se uno di quei test fallisce, il lettore ha perso la capacità di
leggere un formato che è già in circolazione**: rigenerare la fixture non è la
correzione, è la cancellazione della prova.

Si rigenera solo per **congelare un formato nuovo**:

```console
bash scripts/make-format-fixtures.sh                   # tutte
bash scripts/make-format-fixtures.sh legacy-envelope1  # una sola
```

Lo script costruisce la fixture legacy da un `git worktree` sul tag
corrispondente, quindi richiede un albero git completo.

## Come far girare i gate

```console
make check                 # gate unico (fmt, vet, lint, build, test, race, deps-check, docs-check, proto-check, vuln)
make cover PKG=./pkg/…     # copertura del pacchetto della fase
make e2e PHASE=NN          # e2e della fase
make vuln                  # solo govulncheck
make build-all             # 8 piattaforme
```

### Condizioni d'ambiente dei gate

Tre target del gate non sono eseguibili ovunque, e il modo in cui falliscono va
saputo prima di interpretarne l'esito.

| Target | Richiede | Se manca |
| --- | --- | --- |
| `race` | `CGO_ENABLED=1`, **socket locali** e una **cache Go scrivibile** | fallisce, e non per una race |
| `vuln` | `govulncheck` in `$HOME/go/bin` e accesso a <https://vuln.go.dev> | fallisce |
| `proto-check` | `protoc` 27.3 e `protoc-gen-go` v1.34.2 | **SKIP con exit 0**, salvo `BACKIMAGE_REQUIRE_PROTOC=1` |

`race` è il caso che si presta al malinteso. I test di trasporto aprono socket su
`localhost` e il compilatore del race detector scrive nella cache Go: dentro una
sandbox che vieta la rete locale o monta la cache in sola lettura il target esce
rosso **senza aver misurato nulla**, e l'esito non va letto come una race trovata.
Va rieseguito fuori dalla sandbox prima di trarre conclusioni.

Eseguito fuori sandbox il 10 settembre 2026 su go1.26.6: **21 package `ok`, exit 0,
nessun `DATA RACE`**.

In CI il gate gira dentro `make check` nel job `quality`, senza `continue-on-error`
e senza `if:` condizionale: un `race` rosso ferma la pipeline e blocca i job
`cross-build` ed `e2e`, che ne dipendono via `needs`. Nessuna esclusione, nessun
`-skip`.
