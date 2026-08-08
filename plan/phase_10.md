# Fase 10 — Dedup content-defined (backup incrementali)

**Obiettivo**: rendere il secondo backup di dati quasi invariati economico. Il registry deduplica per digest: se i layer invariati mantengono lo stesso digest, non vengono ricaricati.

**Tensione nota e accettata**: il requisito D02 (`docker pull` e `docker run` devono funzionare) impone che un layer sia un tar valido; quindi la granularità della dedup è il **layer**, non il chunk. Questa fase abbassa la dimensione del layer e ne rende **content-defined il confine**, ottenendo un incrementale reale ma a grana più grossa di uno strumento tipo restic.

**Riferimento decisioni**: D02, D05, D06, D15.

---

## 10.1 Splitter content-defined

**Agente: Sonnet**

### File: `pkg/chunk/cdc.go`

```go
// CDCParams tunes the content-defined chunker.
type CDCParams struct {
	Min, Avg, Max int64 // bytes; defaults 1 MiB / 4 MiB / 16 MiB
	Polynomial    uint64 // fixed for reproducibility across versions and machines
}

// DefaultCDCParams returns the parameters used by --dedup.
func DefaultCDCParams() CDCParams

// NewCDC splits r on content-defined boundaries.
func NewCDC(r io.Reader, p CDCParams) Splitter
```

### Prescrizioni
- Usare `github.com/restic/chunker` (rolling hash di Rabin) con un **polinomio fisso**, costante nel codice e registrato in `manifest.json` sotto `chunking.polynomial`. Un polinomio casuale per backup renderebbe impossibile la dedup fra backup diversi: è l'errore da non commettere.
- I parametri (`min`, `avg`, `max`, `polynomial`) fanno parte dell'identità della dedup: cambiarli fra due backup li rende non deduplicabili. Il manifest li registra e `backup --dedup` **eredita i parametri dall'ultimo backup dello stesso repo** se ne trova uno, invece di usare i default. Se l'utente li forza a valori diversi, avvisare che la dedup con i backup precedenti sarà nulla.
- L'interfaccia `Splitter` non cambia: `--dedup` seleziona `NewCDC` al posto di `NewFixed`.

### Test
- determinismo: lo stesso input dà gli stessi confini, in due esecuzioni e in due processi;
- **proprietà di spostamento**: inserendo 1 byte all'inizio di un flusso da 100 MiB, almeno il **90 %** dei chunk successivi resta identico (è *il* test che dimostra che il CDC funziona);
- rispetto di min/max su input patologici (tutti zeri, casuale, altamente ripetitivo);
- dimensione media entro il ±25 % di `Avg` su input casuale.

---

## 10.2 Confini di layer content-defined

**Agente: Sonnet**

### Problema
Se i layer si chiudono a una dimensione fissa (es. ogni 64 MiB), l'inserimento di dati sposta il confine di tutti i layer successivi e ne cambia il digest, annullando il beneficio del CDC a livello di chunk.

### File: `pkg/chunk/layerbound.go`

```go
// LayerBoundary decides where a layer ends.
type LayerBoundary interface {
	// ShouldClose reports whether the layer should end after the given chunk.
	ShouldClose(layerBytes int64, chunkDigest [32]byte) bool
}

// NewFixedBoundary closes layers at a fixed size (phases 02–09 behaviour).
func NewFixedBoundary(size int64) LayerBoundary

// NewContentBoundary closes a layer when the chunk digest satisfies a
// probabilistic condition, so boundaries follow content, not offsets.
func NewContentBoundary(target, min, max int64) LayerBoundary
```

### Algoritmo prescritto per `NewContentBoundary`

```
mask = (1 << ceil(log2(target/avgChunk))) - 1
ShouldClose(layerBytes, digest):
    if layerBytes >= max:                       return true   // limite duro
    if layerBytes <  min:                        return false // evita layer minuscoli
    return (binary.BigEndian.Uint64(digest[:8]) & mask) == 0
```

`min = target/4`, `max = target*4`.

### Vincolo con il planner
Il numero di layer resta soggetto al limite di 118 (D05). Con confini probabilistici il numero non è noto in anticipo: prescrizione operativa —
- si stima `layerCount ≈ storedBytes / target`;
- se durante la produzione si raggiungono **110** layer, si passa a `NewFixedBoundary(remaining / 8)` per i restanti, garantendo il rispetto del limite;
- il passaggio va registrato in un warning e nel manifest (`chunking.boundaryFallback: true`).

### Test
- inserimento di 1 byte all'inizio di un flusso da 4 GiB: almeno il **70 %** dei layer resta con digest identico;
- il numero di layer non supera mai 118 su 50 input casuali di dimensioni fra 1 GiB e 10 TiB;
- il fallback si attiva e viene registrato.

---

## 10.3 DEK stabile e nonce convergente

**Agente: Sonnet** — *revisione crittografica di Opus obbligatoria*

### Problema
Con DEK e nonce casuali per backup, due chunk identici producono blob diversi: nessuna dedup. Serve che la cifratura sia deterministica rispetto al contenuto.

### Soluzione prescritta
1. `--dedup` implica il riuso della **stessa** `KeyMaterial` fra i backup dello stesso repository.
2. Il client, prima del backup, prova a leggere `keys*.age` dall'ultimo backup del repo e a riaprirlo con la passphrase/identità fornita. Se ci riesce, riusa quella `KeyMaterial`; altrimenti ne genera una nuova (primo backup, o passphrase cambiata).
3. Il nonce passa in modalità `NonceConvergent` (03.4): `nonce = HMAC-SHA256(NonceKey, plainSHA)[:12]`.
4. `manifest.json` registra `encryption.nonceMode: "convergent"` e `encryption.keyFingerprint` = `SHA-256(DEK)` **troncato a 8 byte** — serve solo a verificare che due backup condividano la chiave, non rivela la chiave.

### Conseguenza sulla riservatezza, da documentare in `docs/security.md`
Chi osserva il registry può dedurre **quali chunk sono condivisi** fra due backup, e quindi stimare quanto sono cambiati i dati. Non può leggerne il contenuto. È il prezzo intrinseco della dedup lato client ed è il motivo per cui `--dedup` **non** è attivo di default.

### Prescrizioni
- `--dedup` con `--no-encrypt`: nessun problema, il nonce non esiste.
- `--dedup` con passphrase diversa dall'ultimo backup → avviso esplicito: *"passphrase diversa: la dedup con i backup precedenti non sarà possibile"*, e si procede con una nuova chiave.
- Vietato riusare la stessa DEK con nonce **casuale** e poi passare a convergente sullo stesso repo senza cambiare chiave: sarebbe un riuso di nonce. Prescrizione: la modalità nonce è **immutabile** per una data `KeyMaterial`; se cambia, si genera una nuova `KeyMaterial`. Test obbligatorio.

### Test
- due backup consecutivi con la stessa passphrase → stessa `keyFingerprint`;
- passphrase diversa → fingerprint diversa, avviso presente;
- passaggio da random a convergente sullo stesso repo → nuova chiave generata, mai riuso di nonce (test che confronta i nonce di tutti i blob dei due backup);
- chunk identici in due backup → blob byte-identici.

---

## 10.4 Skip via `HEAD` e statistiche

**Agente: Sonnet**

Il salto dei blob già presenti esiste già dalla fase 05.4. Qui si tratta di **misurarlo e mostrarlo**.

### Prescrizioni
- Output di `backup` con `--dedup`:
  ```
  dedup    38/48 layer già presenti sul registry (18,2 GiB risparmiati)
  upload   10 layer, 2,1 GiB, 4m12s
  ```
- In `--json`: `"skippedBlobs":38, "skippedBytes":19541180416, "uploadedBytes":2254857830`.
- Nuovo comando `backimage repo stats <REPO>` (anticipo della fase 11): mostra quanti blob sono condivisi fra i tag di un repository e lo spazio effettivo occupato.

---

## 10.5 End-to-end della dedup

**Agente: Sonnet**

### File: `test/e2e/phase_10.sh`

```
1.  crea un albero da 4 GiB con file di varie dimensioni
2.  backimage backup ./tree --repo localhost:5000/e2e/dd --tag t1 --dedup
    registra bytesUploaded_1
3.  modifica l'1 % dei dati: tocca 40 MiB distribuiti su file diversi,
    aggiunge un file da 10 MiB, cancella un file da 5 MiB
4.  backimage backup ./tree --repo localhost:5000/e2e/dd --tag t2 --dedup
    registra bytesUploaded_2
5.  ASSERZIONE:  bytesUploaded_2 < bytesUploaded_1 * 0,25
6.  docker pull localhost:5000/e2e/dd:t2 && docker run … tar > out.tar
7.  confronto con l'albero modificato → ZERO differenze
8.  docker pull localhost:5000/e2e/dd:t1 && docker run … tar > out1.tar
    confronto con l'albero ORIGINALE (salvato a parte) → ZERO differenze
    ← dimostra che la dedup non ha corrotto il backup precedente
9.  backimage verify localhost:5000/e2e/dd:t1 e :t2 → entrambi exit 0
```

Il passo 5 con soglia al 25 %: con confini di layer content-defined su un target di 64 MiB e una modifica dell'1 % distribuita, il risultato atteso è nell'ordine del 10–20 %. La soglia al 25 % lascia margine senza rendere il test inutile. **Se il risultato reale supera il 25 %, non alzare la soglia: indagare i confini di layer.**

### Definition of Done
- [ ] `make e2e PHASE=10` esce 0
- [ ] i passi 7 e 8 danno zero differenze entrambi

---

## Gate di fase 10

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/chunk/...` | ≥ 90 % |
| G8 | `make e2e PHASE=10` | exit 0 |
| **GS-10.1** | CDC: 1 byte inserito in 100 MiB | ≥ 90 % dei chunk invariati |
| **GS-10.2** | confini di layer: 1 byte inserito in 4 GiB | ≥ 70 % dei layer invariati |
| **GS-10.3** | limite dei layer su 50 input casuali | sempre ≤ 118 |
| **GS-10.4** | secondo backup con 1 % modificato | byte caricati < 25 % del primo |
| **GS-10.5** | restore del backup precedente dopo il secondo | zero differenze |
| **GS-10.6** | nonce su due backup con la stessa DEK | nessun riuso con contenuti diversi |
| **GS-10.7** | `--dedup` non attivo di default | verificato nei test dei flag |
| G10 | `docs/dedup.md`, aggiornamento di `docs/security.md` | presenti |
| G11 | revisione Opus (crittografica per 10.3) | **obbligatoria** |

**Deliverable documentali**
- `docs/dedup.md`: come funziona, quanto si risparmia in pratica (numeri dell'e2e), i parametri e il vincolo che devono restare stabili, l'interazione con la cifratura, perché la grana è il layer e non il chunk, confronto onesto con restic/kopia.
- `docs/security.md`: nuova sezione sul compromesso della modalità convergente.

**Rischi noti**
- Riuso di nonce: è l'errore crittografico più grave possibile in questa fase. Il test `GS-10.6` è la difesa e non va indebolito per nessun motivo.
- Se i parametri CDC cambiassero fra due versioni di backimage, tutti i backup successivi perderebbero la dedup senza che nessuno se ne accorga. Sono costanti: modificarle è un breaking change che richiede l'approvazione di Opus e una nota nel `CHANGELOG.md`.
