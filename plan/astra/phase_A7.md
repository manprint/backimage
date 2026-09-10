# Fase A7 — Limiti su allocazioni, metadati e decompressione

**Obiettivo**: nessun campo proveniente da una sorgente modificabile può decidere quanta memoria,
quanto tempo o quante risorse il lettore spende.

**Rilievi coperti**: A09.

**Nota sulla provenienza**: il corpo di A09 manca nel documento consegnato. I quattro punti qui
sotto sono la nostra analisi del sorgente, non il testo del team: quando arriverà il documento
completo va fatta la riconciliazione.

---

## A7.1 Allocazioni derivate da campi pubblici

**Agente: Sonnet**

### Il difetto, verificato

`pkg/recovery/recovery.go:346-349`:

```go
if c.Sb > int64(int(^uint(0)>>1)) {
	return nil, fmt.Errorf("chunk %d too large", i)
}
buf := make([]byte, int(c.Sb))
```

`c.Sb` è la dimensione memorizzata dichiarata da `chunks.json`, che è **pubblico e modificabile**.
L'unico guard è il massimo intero. Un `chunks.json` che dichiara 100 GiB provoca un tentativo di
allocazione da 100 GiB. Lo stesso schema in `pkg/restore/source.go:334-336`.

Non viene fatto alcun confronto con i dati che il backup stesso già contiene: `layer.StoredBytes`
(usato in `source.go:311` per la materializzazione) e `Chunking.MaxChunkBytes`
(`pkg/index/model.go:115`).

### Intervento

- `Sb` deve essere ≤ `layer.StoredBytes` del layer che lo contiene e ≤ `MaxChunkBytes` più
  l'overhead dell'envelope; la somma delle `Sb` di un layer deve coincidere con `StoredBytes`.
- Il controllo precede l'allocazione, e l'errore è di classe integrità/formato.
- Per `LocalSource`, confrontare anche con la dimensione reale del file di layer: `os.Stat` è già
  disponibile sul percorso.

## A7.2 Blob di metadati letti senza tetto

**Agente: Sonnet**

`pkg/index/model.go:384` (`ReadIndex`) e `pkg/index/private.go:145` (`ReadPrivate`) fanno
`io.ReadAll` su un reader che proviene dall'immagine. Nessun limite.

- `io.LimitReader` con un tetto derivato dal descrittore OCI del blob quando disponibile, e un
  tetto assoluto configurabile altrimenti.
- Superare il tetto è un errore di formato, non un `OOM`.

## A7.3 Decompressione senza limite di memoria

**Agente: Sonnet**

`pkg/index/model.go:370` e `:401` costruiscono i reader zstd con `zstd.WithDecoderConcurrency(1)` e
nient'altro. Manca `WithDecoderMaxMemory`, il cui **default dichiarato dalla libreria è 64 GiB**
(`go doc github.com/klauspost/compress/zstd.WithDecoderMaxMemory`): un blob di metadati ostile è
una bomba di decompressione.

- `WithDecoderMaxMemory` esplicito su **tutti** i reader zstd che leggono dati non fidati, non solo
  questi due: verificare `pkg/compress` e il percorso di materializzazione dei layer
  (`pkg/restore/source.go:395`).
- Il rapporto di espansione atteso va confrontato con le dimensioni dichiarate nei metadati, così
  che una discrepanza sia un errore di formato prima di essere un consumo.

## A7.4 L'indice non ha limiti di forma

**Agente: Sonnet**

`pkg/index/model.go:479-501`, `validateEntries`, controlla path non vuoto, tipo noto, size e offset
non negativi, modo parsabile, sha256 esadecimale, link target presente. **Non** controlla:

- numero massimo di entry;
- lunghezza massima del path e del link target;
- unicità dei path;
- **monotonia di `TarOffset`** — e `pkg/recovery/partial.go:81-87` la *assume*, perché calcola la
  fine di un'entry dall'offset della successiva.

Interventi: cap su conteggio e lunghezze, rifiuto dei duplicati, verifica dell'ordinamento degli
offset e della loro copertura entro `contentEnd`. Quest'ultimo controllo è quello che rende sicuro
il calcolo del recupero parziale, quindi va coordinato con A3.2.

## A7.5 Conteggi e risorse sul percorso remoto

**Agente: Sonnet**

Il protocollo ha già i campi di limite (`max_bytes`, `max_layer_bytes` in
`pkg/protocol/backimage.proto:17,104`, usati in `pkg/server/session.go:69` e
`pkg/server/stream.go:80`, applicati alla riga 374). Va
completato il lato client e il conteggio delle risorse per sessione:

- numero massimo di scope e di goroutine di rinnovo per sessione (coordinato con A4.1, che
  introduce il punto di validazione);
- numero massimo di layer e di frame annunciati, verificato prima di allocare.

---

## Accettazione della fase

Per ciascuno dei cinque punti, una fixture ostile che oggi provoca allocazione, consumo o
assunzione errata, e dopo il fix produce un errore di formato **prima** di qualunque allocazione
significativa. I test misurano la memoria del processo, non solo l'esito.

Nessun tetto va scelto a caso: ognuno deve derivare da un valore che il backup stesso dichiara, o
essere configurabile con un default motivato nella documentazione.

**e2e**: `test/e2e/phase_A7.sh` — metadati ostili su un layout locale, verifica che il rifiuto sia
immediato e che il processo non superi una soglia di memoria.

**Documentazione**: `docs/ARCHITECTURE.md` per i tetti e la loro derivazione, `CHANGELOG.md`.

---

## Uscita di fase

- Nessun campo di una sorgente modificabile decide quanta memoria allocare.
- Ogni decompressione di dati non fidati ha un limite esplicito.
- Le assunzioni del recupero parziale sono verificate, non presunte.
