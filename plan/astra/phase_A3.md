# Fase A3 — Integrità dello stream e costo delle letture

**Obiettivo**: nessun byte non verificato raggiunge il consumatore, e leggere un backup costa
lineare nella sua dimensione invece di quadratico.

**Rilievi coperti**: A04, A14, A15, A16. **Decisione**: DA-01.

**Perché insieme**: i quattro rilievi vivono negli stessi due file, `pkg/recovery/recovery.go`
(più `partial.go`) e `pkg/restore/source.go`. A04 e A14 si chiudono con **lo stesso** intervento.

---

## A3.1 Verifica prima dell'emissione, per doppia decompressione

**Agente: Sonnet**. **Rilievo**: A04. **Decisione**: DA-01.

### Il difetto, verificato

`pkg/recovery/recovery.go:440-452`, in `StreamTar`:

```go
n, copyErr := io.Copy(w, r)          // w = io.MultiWriter(dst, h)
...
if n != c.Pb { return ...ErrIntegrity... }
if verify && !digestMatches(c.Ps, h.Sum(nil)) { return ...ErrIntegrity... }
```

La review misura **10.752 byte** consegnati al destinatario prima che `crypt.ErrIntegrity` venga
restituito. Un tag GCM invalido viene respinto prima, in `PlainChunk`; il caso che conta è un blob
**validamente autenticato** ma proveniente da un'altra posizione o da un altro backup sotto la
stessa chiave dedup. La difesa contro quel riuso è appunto il digest plaintext del blob private
(vedi il commento di `mustVerify`, `recovery.go:404-418`) — e arriva dopo gli effetti.

### Perché doppia decompressione e non spool

`StoredChunk` (`recovery.go:322-352`, allocazione alla riga 349) legge l'intero chunk memorizzato in
RAM, e `PlainChunk` mantiene `payload` in memoria decomprimendo da `bytes.NewReader(payload)`. Il
compresso è già tutto in memoria: si può rileggerlo due volte senza costo di spazio.

Limiti del chunk plaintext, dal codice: `pkg/chunk/plan.go:88` `clampChunkBytes` = `layerBytes/64`
con clamp `[1 MiB, 64 MiB]` (16 MiB con il target di layer di default); `pkg/chunk/cdc.go:31`
default CDC min 1 MiB / avg 4 MiB / max 16 MiB; tetto assoluto 1 GiB (`pkg/chunk/split.go:36`).

Le tre alternative valutate: spool in RAM (+1 chunk, 16–64 MiB), spool su file temporaneo
(disco, e collide con la regola di spazio temporaneo corretta in `e13a8b2`), doppia decompressione
(nessuno dei due, +1 passata zstd a 1–3 GB/s). Scelta: **doppia decompressione**.

### Intervento

**Ostacolo da rimuovere prima**: `PlainChunk` (`recovery.go:358-393`) non espone `payload`. Lo
incapsula in un `bufferedReader` (`recovery.go:396-405`) il cui `Close()` esegue `clear(r.data)`
alla riga 403. Con l'API attuale la seconda passata non ha più i byte: al primo `Close`
il buffer è azzerato.

Serve quindi un accessorio interno — per esempio `plainChunkPayload(ctx, i) ([]byte, compress.ID, error)` —
che restituisca il compresso già autenticato e il codec, lasciando al chiamante la decisione su
quante volte decomprimerlo e quando azzerarlo. `PlainChunk` resta come API pubblica costruita
sopra di esso, così i chiamanti esterni non cambiano.

- Passata 1: `codec.NewReader(bytes.NewReader(payload))`, `io.Copy(io.MultiWriter(io.Discard, h), r)`,
  confronto di `n` con `c.Pb` e del digest con `c.Ps`. Chiudere il reader, **non** azzerare il
  payload.
- Passata 2: solo se la passata 1 è passata, nuovo reader sullo stesso `payload`, `io.Copy(dst, r)`.
  Azzerare il payload al termine, con `defer`, in modo che valga anche sui percorsi di errore.
- Vale per `StreamTar` e per il percorso parziale. `plainChunkBytes`, che il percorso selettivo usa
  e che già verifica prima di emettere, resta invariato.
- Riportare nel progresso la doppia passata, per non far leggere come regressione il tempo in più.

### Atomicità: dire cosa si garantisce

La review chiede di distinguere l'atomicità del chunk, del singolo file e dell'intero restore.
Questa sub-fase chiude **solo** quella del chunk. Le altre due restano dichiarate come non
garantite: i file già pubblicati prima di un errore vanno **rendicontati** esplicitamente
nell'output e nel JSON, invece di essere lasciati dedurre. La pubblicazione atomica della
destinazione è materiale per una fase futura, non per questa.

## A3.2 Recupero parziale senza memoria proporzionale al file più grande

**Agente: Sonnet**. **Rilievo**: A14.

### Il difetto, verificato

`pkg/recovery/partial.go:88` accumula l'intera entry prima di scriverla, tramite
`readRange` (`:116`, allocazione alla riga 117): `out := make([]byte, 0, end-start)`. Un file da
50 GB dentro il backup significa 50 GB residenti. Il buffer non è accidentale: serve a poter
scartare un'entry i cui chunk sono danneggiati senza aver già scritto byte.

### Intervento

Lo stesso schema di A3.1 elimina il buffer, con una precisazione che l'implementatore deve avere
chiara: i chunk sono **già** verificati da `plainChunkBytes` quando `load` li carica, quindi lo
scopo della prima passata non è verificare di nuovo, è **sapere in anticipo se tutti i chunk che
coprono l'entry sono caricabili**, prima di scrivere il primo byte.

- Passata 1: percorrere i chunk del range dell'entry chiamando `load` su ciascuno e scartandone il
  contenuto. Se uno è danneggiato, l'entry va in `report.Skipped` senza che nulla sia stato scritto:
  è esattamente la garanzia odierna.
- Passata 2: ripercorrere lo stesso range scrivendo su `dst`.
- **Costo**: ogni chunk dell'entry viene caricato due volte, cioè decifrato e decompresso due
  volte, perché la cache di `load` ne tiene uno solo (`cacheIndex`, `partial.go:50-73`). Per
  un'entry contenuta in un chunk il costo è nullo grazie alla cache; per un'entry che ne attraversa
  molti è un raddoppio. È il prezzo di non bufferizzare, e va misurato e dichiarato.
- La cache a un solo chunk va **mantenuta**: serve alle entry adiacenti che vivono nello stesso
  chunk, che sono la maggioranza. Non sostituirla con un accesso diretto.

## A3.3 Letture quadratiche sul percorso locale

**Agente: Sonnet**. **Rilievo**: A15.

### Il difetto, verificato

`pkg/recovery/recovery.go:343`, in `StoredChunk`:

```go
if _, err := io.CopyN(io.Discard, r, b.offsets[i]); err != nil {
```

Per ogni chunk si **rilegge e scarta** il layer dall'inizio fino al suo offset. Layer da 1 GiB con
chunk da 16 MiB, 64 chunk: ~32 GiB letti invece di 1 GiB, cioè n²/2. Colpisce `LocalSource` e
quindi il percorso dell'immagine autoestraente.

### Intervento

`LocalSource.Open` (`recovery.go:48`) restituisce il risultato di `os.Open`, cioè un `*os.File`,
che è `io.Seeker`. Type-assert su `io.Seeker` e usare `Seek(offsets[i], io.SeekStart)`; `CopyN`
resta come fallback per sorgenti non seekable. Nessun cambio di interfaccia pubblica.

## A3.4 La cache non ricostruisce il layer per ogni chunk

**Agente: Sonnet**. **Rilievo**: A16.

### Il difetto, verificato

`pkg/restore/source.go:343` `materialize`: alla riga 439, quando la cache è disabilitata
(`s.cacheSize < 0`) o il layer supera `s.cacheSize`, la funzione restituisce un file temporaneo con
`ephemeral=true`; e `Blob` (`:293`) alla riga 323 fa `defer os.Remove(path)` dopo aver letto **un
solo chunk**. Quindi per ogni chunk: download del layer, decompressione (riga 393), scrittura su
disco, lettura di 16 MiB, cancellazione. Un layer da 1 GiB con 64 chunk significa 64 GiB di I/O e
64 decompressioni.

### Intervento

- Il file effimero diventa di durata **layer**, non di durata chunk: `imageSource` tiene il layer
  corrente materializzato e lo libera quando la lettura passa al successivo. `StreamTar` consuma i
  chunk in ordine, quindi un solo layer vivo alla volta è sufficiente.
- **Attenzione ai percorsi che non leggono in ordine**: il restore selettivo
  (`StreamSelectedTar`) e il parziale saltano fra entry, e quindi possono alternare layer. Con una
  sola posizione viva si torna a ricostruire ad ogni salto. Ordinare gli accessi per layer dove la
  semantica lo consente, e negli altri casi tenere una piccola mappa di layer vivi con un tetto
  esplicito, non una sola posizione.
- La politica di cache (`cacheSize`, `prune`) resta quella di oggi: qui non si decide *se* tenere,
  si evita di **ricostruire**.
- `VerifyStored` (`pkg/restore/verify_stored.go:62`, `verifyOneLayer:127`) lavora già per layer:
  verificare che la modifica non ne peggiori il percorso, e riusare la stessa materializzazione.

**Misura di accettazione**: contare gli invocazioni di `materialize` e i byte decompressi durante un
restore di N chunk su M layer. Atteso: M, non N.

---

## Accettazione della fase

Per A04, dalla review: nessun byte di un chunk respinto deve raggiungere il consumatore; fixture
con **riuso di blob validi provenienti da backup diversi sotto la stessa chiave**, tag GCM valido e
digest atteso differente; i file già pubblicati devono essere esplicitamente rendicontati se non si
adotta una transazione sull'intero restore.

Per A14: recupero parziale di un backup contenente un file molto più grande del chunk, con memoria
del processo misurata e indipendente dalla dimensione del file.

Per A15 e A16: contatori di byte letti e di materializzazioni, con soglie che fallirebbero sul
comportamento odierno.

**e2e**: `test/e2e/phase_A3.sh` — restore con blob spostato fra backup, restore parziale con file
grande, misura delle letture su un backup multi-layer.

**Documentazione**: `docs/TROUGHPUT_IMPROVE.md` (le due voci di costo qui rimosse), `docs/backup.md`
per la nota sul tempo di restore, `CHANGELOG.md`.

---

## Uscita di fase

- Nessun byte non verificato raggiunge tar, stdout o filesystem.
- Il recupero parziale ha memoria pari a un chunk.
- Leggere un backup costa una passata per layer.
