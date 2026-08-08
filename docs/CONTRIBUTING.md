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

## Come far girare i gate

```console
make check                 # gate unico (fmt, vet, lint, build, test, race, deps-check, docs-check)
make cover PKG=./pkg/…     # copertura del pacchetto della fase
make e2e PHASE=NN          # e2e della fase
make build-all             # 8 piattaforme
```