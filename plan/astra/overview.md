# Piano astra — rimedio ai rilievi della review tecnica del 9 settembre 2026

**Origine**: `astra-analisys.md` (root del repo), review di quality su `main` @ `9260ea5`, v0.4.0.
**Numerazione**: fasi `A0`–`A7`, indipendenti dalla numerazione storica `plan/phase_00…13`.
**Stato**: vedi `resume.md`. Nessuna fase iniziata.

---

## 1. Perché questo piano esiste

La review riproduce un bypass dell'autenticazione sul percorso di lettura cifrato (A01, P0) e
altri diciannove rilievi fra sicurezza, perdita dati, prestazioni e operatività. Il documento
consegnato dal team è **troncato**: il corpo si ferma ad A08, quindi A09–A20 esistono solo nella
tabella riassuntiva, e mancano la sezione 8 (i 13 test `TestAstra…`, applicati con
`go test -overlay` e non presenti nel repo) e l'esito finale del race detector.

Tutti i venti rilievi sono stati **riverificati sul sorgente** prima di scrivere questo piano.
Per A01–A08 le riproduzioni del team sono confermate riga per riga. Per A09 e A14–A20, dove il
corpo del documento manca, l'analisi e i criteri di accettazione qui sotto sono nostri, non del
team: quando arriveranno il documento completo e gli overlay, i criteri delle fasi A3, A6 e A7
vanno riconciliati.

## 2. Verifiche di partenza, eseguite il 10 settembre 2026

| Controllo | Esito |
| --- | --- |
| `CGO_ENABLED=1 go test -race -p 1 -count=1 ./...` | **Verde**: 21 package `ok`, exit 0, nessun `DATA RACE`. Il dubbio della review sul gate race è chiuso |
| `govulncheck ./...` | **15 advisory stdlib tutte raggiungibili dal codice**, tutte chiuse da `go1.26.6`. Fra queste `archive/tar` (GO-2026-4869), 4 su `crypto/x509`, 3 su `crypto/tls`, 3 su `net/http`. Altre 7 negli import e 11 nei moduli richiesti non risultano chiamate |
| `make check` | Si ferma su `lint` **solo in locale**: `.golangci.yml` è in schema v1 e il binario locale è `v2.1.6`. In CI il gate passa perché `ci.yml:15` installa `golangci-lint@v1.64.8`, coerente con la configurazione |
| `proto-check` | Non eseguibile: `protoc` assente (`protoc-gen-go` presente in `~/go/bin`) |
| Asset incorporati | `internal/embedded/backimage-selfextract-linux-{amd64,arm64}` datati **23 agosto**, checkout del **9 settembre**. `Makefile:17` non include `selfextract` nella catena di `check`; CI e release invece eseguono `make embed` (`ci.yml:26`, `release.yml:53,56`), quindi il rischio è sugli esiti locali, non sugli artefatti pubblicati |
| Compatibilità dell'opener stretto (A01) | **Nessun rischio**: il sealer nasce solo con la chiave (`pkg/backup/pipeline.go:894`, `pkg/server/stream.go:304`, guard presente dal commit `157566b`), e `crypt.NewSealer(nil)` ha come unici chiamanti i test. Nessuna release ha mai prodotto blob `aeadNone` dentro un backup cifrato |
| Profilo degli e2e | Nessuno degli 11 script usa `--privileged` o monta `docker.sock`; `test/e2e/phase_06.sh:89,94` esegue l'autoestraente con `--user`. L'hardening della documentazione **non rompe alcun e2e** |

## 3. Decisioni congelate

| # | Decisione | Motivo |
| --- | --- | --- |
| DA-01 | **A04 si risolve con doppia decompressione**, non con spool su disco | Il payload compresso è già interamente in RAM (`pkg/recovery/recovery.go` `StoredChunk`/`PlainChunk`): passata 1 per hash e dimensione, passata 2 per l'emissione. Zero disco, nessun conflitto con la regola sullo spazio temporaneo corretta in `e13a8b2` |
| DA-02 | **A07: bearer permanente rifiutato**, con opt-in esplicito `--forward-static-token` | Etichettare un token permanente come delega limitata è una promessa non mantenuta. Si accetta di rompere i setup che usano `AuthConfig.RegistryToken` in modalità remota, con errore prima dell'upload |
| DA-03 | **Hardlink il cui primo nome è escluso dal restore: skip + report** | Elimina alla radice la classe A02. Si perde la fedeltà odierna (copia dal disco) in cambio dell'impossibilità di aprire pathname fuori dal set ripristinato. In `--strict` resta fatale |
| DA-04 | **A08: digest atteso più hardening**, non firma nativa | `--expect-digest` sul binario host rifiuta prima di ricevere la passphrase; il cleanup Docker esce dall'autoestraente; il profilo confinato diventa l'esempio primario. La firma cosign delle immagini utente resta una feature da decidere a parte, anche perché collide con il vincolo di dipendenze di `scripts/check-deps.sh:32` |
| DA-05 | **A10/A11: nota nel changelog e nuova release**, senza yank | La 0.4.1 pinza `toolchain go1.26.6`, mette gli asset nella catena di build e porta i gate verdi; il changelog dichiara che le release precedenti sono anteriori ai fix e costruite con una stdlib con 15 advisory raggiungibili |

## 4. Copertura dei rilievi

| ID | Priorità review | Fase | Nota |
| --- | --- | --- | --- |
| A01 | P0 | A1 | Con la matrice di accettazione completa del documento |
| A02 | P1 | A2 | DA-03 |
| A03 | P1 | A2 | `os.Root` e descrittori |
| A04 | P1 | A3 | DA-01 |
| A05 | P1 | A6 | Un solo bump di formato |
| A06 | P1 | A4 | |
| A07 | P1 | A4 | DA-02 |
| A08 | P1 | A5 | DA-04 |
| A09 | P1 | A7 | Quattro punti distinti |
| A10 | P1 | A0 | `go1.26.6` chiude tutte le 15 |
| A11 | P1 | A0 | Precondizione di tutto il resto |
| A12 | P1 | A1 | |
| A13 | P1 | A1 | |
| A14 | P2 | A3 | Stesso intervento di A04 |
| A15 | P2 | A3 | |
| A16 | P2 | A3 | |
| A17 | P2 | A2 | |
| A18 | P2 | A2 | Riclassificato: perdita dati, non solo incompletezza |
| A19 | P2 | A6 | Deve viaggiare col bump, non prima |
| A20 | P2 | A6 | |

Punti delle sezioni 2 e 3 della review che non stanno nella tabella dei rilievi:

- **CI solo Linux** (`ci.yml`: tre job, tutti `ubuntu-latest`) → job Windows in A2.
- **e2e Docker/QEMU non eseguiti** → script nuovi in A5.
- **Cosign presente ma sugli archivi di release** (`release.yml:83-90`), non sulle immagini utente → A5, DA-04.
- **README guida al profilo più esposto** (`README.md:12`, `README.it.md:12`, `docs/handbook.it.md:683`) → A5.
- **Review crittografica indipendente della modalità convergente** → **non mitigabile qui**: A6 produce il dossier per un revisore esterno. Resta rischio residuo dichiarato.

## 5. Ordine e mappa sulle release

```
A0  gate e toolchain          ─┐
A1  autenticità e perdita dati ├─→ 0.4.1  (sicurezza, nessun cambio di formato)
A2  confinamento estrazione   ─┘
A3  integrità stream e letture ─┐
A4  delega remota              ├─→ 0.5.0  (cambi di comportamento e flag)
A5  fiducia eseguibile         ─┘
A6  formato autenticato        ─┐
A7  limiti e risorse           ├─→ 0.6.0  (un solo bump di envelope/schema)
                               ─┘
```

Vincoli d'ordine non negoziabili:

1. **A0 precede tutto.** `make check` non rigenera gli asset incorporati (`Makefile:17`), quindi in locale i test possono misurare un estrattore diverso dal codice che si sta scrivendo. CI e release invece eseguono `make embed` (`ci.yml:26`, `release.yml:53,56`), quindi i binari pubblicati **non** contengono un estrattore vecchio: il difetto colpisce la fiducia negli esiti locali, non gli artefatti rilasciati. Va chiuso per primo perché tutte le fasi successive si validano in locale.
2. **A1 precede A6.** L'opener stretto è il presupposto del binding autenticato; invertirli significa scrivere due volte lo stesso codice di validazione.
3. **A19 non può precedere il bump di formato**, perché il fix cambia la derivazione del nonce.
4. **A3 va dopo A0** per il bump di toolchain: la stdlib `archive/tar` aggiornata può rifiutare header che oggi passano, e va misurato prima di riscrivere il percorso di lettura.

5. **A6.0 precede tutto il resto di A6.** Non esistono fixture dei formati rilasciati
   (`pkg/recovery/testdata` è assente, i test costruiscono le fixture in codice): appena il writer
   cambia non si può più produrre un backup del formato vecchio, e i test di retrocompatibilità
   diventano impossibili da scrivere.

## 5.1 Vincoli tecnici verificati

L'elenco dei vincoli accertati su codice e API — cosa `os.Root` copre e cosa no, l'assenza di stamp
nell'asset incorporato, il payload azzerato dalla `Close` di `PlainChunk`, il default di 64 GiB di
zstd, `ExpiresAt` zero già interpretato come token invalido — sta in `resume.md`, sezione "Vincoli
tecnici verificati". Chi implementa lo legge **prima** di aprire una fase: sono i punti dove la
strada ovvia non funziona.

## 6. Politica di gate

Ogni sub-fase è chiusa quando: `make check` è verde (dopo A0 lo è davvero), i test interni della
sub-fase passano, l'e2e indicato passa, e i documenti elencati sono aggiornati nella stessa
commit del codice.

**Come si aggiunge un e2e**: `Makefile:65` esegue `bash test/e2e/phase_$(PHASE).sh`, quindi gli
script nuovi si chiamano `test/e2e/phase_A1.sh`, `phase_A2.sh` e così via, e si lanciano con
`make e2e PHASE=A1` senza toccare il Makefile. **Vanno però aggiunti alla matrice di
`.github/workflows/ci.yml:90`** (`phase: ['00', '01', '04', …]`): uno script non elencato lì non
gira in CI, e la fase risulterebbe chiusa senza essere stata verificata. Nessuna sub-fase si dichiara chiusa con `-skip` o test auto-esclusi: un test
che si auto-esclude senza privilegi non equivale a un gate eseguito, come nota la review.

## 7. Assegnazione degli agenti

- **Opus**: progettazione del formato autenticato (A6), separazione degli opener (A1.1), modello di fiducia dell'autoestraente (A5.1).
- **Sonnet**: implementazione di tutte le sub-fasi, test interni, riscrittura dell'estrazione (A2).
- **Haiku**: sweep documentali, script e2e, migrazione della configurazione di lint, inventario dei riferimenti nei README.
