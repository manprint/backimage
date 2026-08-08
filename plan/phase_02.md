# Fase 02 — `pkg/compress` e `pkg/chunk`

**Obiettivo**: quattro codec dietro un'unica interfaccia registrata, uno splitter a dimensione fissa che produce chunk, e il **planner dei layer** che rispetta il limite di 127 layer di overlayfs aumentando la dimensione del layer invece del numero (decisione D05, confermata dall'utente).

**Riferimento decisioni**: D02, D05, D14, D15.

---

## 02.1 Interfaccia `Codec` e registro

**Agente: Sonnet**

### File: `pkg/compress/codec.go`

```go
// ID identifies a compression algorithm on the wire and in the blob envelope.
type ID uint8

const (
	Store ID = 0
	Gzip  ID = 1
	Zstd  ID = 2
	Xz    ID = 3
	Lz4   ID = 4
)

// Codec compresses and decompresses a byte stream.
type Codec interface {
	// ID returns the wire identifier written into the blob envelope.
	ID() ID
	// Name returns the user-facing name, e.g. "zstd".
	Name() string
	// MediaTypeSuffix returns the OCI layer media type suffix ("gzip", "zstd")
	// or "" when the algorithm has no standard OCI media type.
	MediaTypeSuffix() string
	// Levels returns the valid level range, inclusive.
	Levels() (min, max, def int)
	// NewWriter wraps w. Closing the returned writer flushes but does not close w.
	NewWriter(w io.Writer, level int) (io.WriteCloser, error)
	// NewReader wraps r.
	NewReader(r io.Reader) (io.ReadCloser, error)
}

// Register makes a codec available by name. It panics on duplicate names and
// must only be called from package-level variable initialisation of this package.
func Register(c Codec)

// Get returns the codec registered under name.
func Get(name string) (Codec, error)

// ByID returns the codec for a wire identifier.
func ByID(id ID) (Codec, error)

// Names returns the registered names, sorted.
func Names() []string
```

### Prescrizioni

- **`MediaTypeSuffix()` vuoto ⇒ immagine non conforme a D02.** Chi assembla l'immagine (fase 04) deve rifiutare xz/lz4 quando `--runnable` è attivo (default), con `Hint`: *"xz e lz4 producono layer non standard: usa --compression zstd|gzip, oppure --runnable=false se accetti un'immagine solo pullabile"*.
- Il codec `Store` esiste per test e per dati già compressi (`--compression none`).
- Nessun `init()`: la registrazione avviene in un `var _ = func() bool { Register(...); return true }()` per ogni file di codec, oppure — preferibile — in un unico `registry.go` con un `var builtin = []Codec{…}` e una `func init()` **solo** in quel file (unica eccezione ammessa alla regola 9 dell'harness).

### Test richiesti
- `Get("nonesiste")` → errore che elenca i nomi disponibili;
- `Register` duplicato → panic (test con `recover`);
- `Names()` è ordinato e contiene esattamente 5 voci.

---

## 02.2 Implementazioni dei quattro codec

**Agente: Sonnet**

### File: `pkg/compress/gzip.go`, `zstd.go`, `xz.go`, `lz4.go`, `store.go`

| Codec | Libreria | Livelli (min/max/default) | MediaTypeSuffix |
|---|---|---|---|
| gzip | `klauspost/compress/gzip` | 1 / 9 / 6 | `gzip` |
| zstd | `klauspost/compress/zstd` | 1 / 4 / 2 (mappati su `SpeedFastest…SpeedBestCompression`) | `zstd` |
| xz | `ulikunitz/xz` | 0 / 9 / 6 | *(vuoto)* |
| lz4 | `pierrec/lz4/v4` | 0 / 9 / 1 | *(vuoto)* |
| store | — | 0 / 0 / 0 | *(vuoto, ma vedi nota)* |

Nota su `store`: un layer tar non compresso ha media type OCI valido (`application/vnd.oci.image.layer.v1.tar`), quindi `MediaTypeSuffix()` restituisce la stringa speciale `"none"` e la fase 04 la tratta come conforme.

### Prescrizioni

- `zstd`: usare `zstd.NewWriter(w, zstd.WithEncoderLevel(l), zstd.WithEncoderConcurrency(n))` con `n = min(GOMAXPROCS, 4)`. Il `Close` del writer **non** deve chiudere `w`: usare un wrapper che intercetta `Close`.
- `zstd.NewReader` restituisce un `*Decoder` che non è un `io.ReadCloser`: avvolgerlo (`IOReadCloser()`).
- Validazione dei livelli: fuori range → errore `KindUsage` con il range valido nel messaggio.
- Ogni codec deve gestire un input vuoto (0 byte) senza errori, e produrre un output che si decomprime in 0 byte.

### Test richiesti
- tabella su tutti i codec × {vuoto, 1 byte, 1 MiB casuale, 1 MiB di zeri, 8 MiB di testo ripetuto}: round-trip byte-identico;
- livello fuori range → errore;
- `NewWriter(...).Close()` non chiude il writer sottostante (verificare con un writer che registra la chiusura);
- fuzz (`FuzzRoundTrip`) su input arbitrari per zstd e gzip, 60 s in CI.

### Definition of Done
- [ ] copertura `pkg/compress` ≥ 90 %
- [ ] fuzz senza crash

---

## 02.3 Interfaccia `Splitter` e splitter a dimensione fissa

**Agente: Sonnet**

### File: `pkg/chunk/split.go`

```go
// Chunk is one unit of the plaintext stream.
type Chunk struct {
	Index      int
	PlainBytes int64
	PlainSHA   [32]byte
	Data       []byte // valid only until the next call to Next
}

// Splitter cuts a stream into chunks.
type Splitter interface {
	// Name identifies the strategy: "fixed" or "cdc".
	Name() string
	// Next returns the next chunk, or io.EOF. The returned Data buffer is
	// reused: callers must consume it before calling Next again.
	Next() (*Chunk, error)
}

// NewFixed splits r into chunks of exactly size bytes (the last one may be shorter).
func NewFixed(r io.Reader, size int64) Splitter
```

### Prescrizioni

- `size` valido: da 1 MiB a 1 GiB; fuori range → errore.
- Il buffer è allocato una volta e riusato: nessuna allocazione per chunk (verificabile con `testing.AllocsPerRun`).
- `PlainSHA` è calcolato mentre si riempie il buffer, non con una seconda passata.
- Determinismo: lo stesso input produce sempre gli stessi chunk. Test obbligatorio.

### Test richiesti
- stream da 10 MiB con `size` 4 MiB → 3 chunk (4, 4, 2 MiB), indici 0,1,2;
- stream vuoto → subito `io.EOF`, zero chunk;
- stream esattamente multiplo di `size` → nessun chunk finale vuoto;
- `AllocsPerRun` ≤ 1 su 100 chunk;
- lettore che restituisce dati a pezzi da 1 byte → chunk comunque pieni.

---

## 02.4 Planner dei layer con guardia 127

**Agente: Sonnet** — *cuore della decisione D05*

### File: `pkg/chunk/plan.go`

```go
// LayerLimits encodes the constraints of a runnable OCI image.
type LayerLimits struct {
	MaxDataLayers   int   // 118 by default: 127 overlayfs limit minus binary+metadata+margin
	MaxLayerBytes   int64 // hard registry-side ceiling, default 5 GiB (warning above)
	MinLayerBytes   int64 // default 16 MiB
	TargetLayerBytes int64 // from --max-layer-size, default 1 GiB
}

// DefaultLimits returns the limits used when the user gives no override.
func DefaultLimits() LayerLimits

// Plan describes how chunks map onto image layers.
type Plan struct {
	LayerBytes  int64
	LayerCount  int
	ChunkBytes  int64
	Warnings    []string
}

// PlanLayers computes a layout for a stream of the given estimated size.
// It never exceeds limits.MaxDataLayers: when the estimate is large it grows
// LayerBytes instead of LayerCount (decision D05).
func PlanLayers(estimatedStoredBytes int64, limits LayerLimits) (Plan, error)
```

### Algoritmo prescritto (implementare esattamente così)

```
1. if estimatedStoredBytes <= 0        -> LayerCount=1, LayerBytes=MinLayerBytes, fine
2. layerBytes = TargetLayerBytes
3. layerCount = ceil(estimatedStoredBytes / layerBytes)
4. if layerCount > MaxDataLayers:
        layerCount = MaxDataLayers
        layerBytes = ceil(estimatedStoredBytes / layerCount)
        warning: "backup grande: dimensione layer portata a <X> per restare
                  entro <MaxDataLayers> layer (limite overlayfs)"
5. if layerBytes < MinLayerBytes:
        layerBytes = MinLayerBytes
        layerCount = ceil(estimatedStoredBytes / layerBytes)   // <= MaxDataLayers per costruzione
6. if layerBytes > MaxLayerBytes:
        warning: "layer da <X>: alcuni registry rifiutano blob così grandi"
7. ChunkBytes = clamp(layerBytes / 64, 1 MiB, 64 MiB)
8. ritorna
```

Nota su `estimatedStoredBytes`: è una **stima**, perché la dimensione compressa non è nota in anticipo. La fase 05 la calcola come `bytesRaw * fattoreStimato` con fattore per codec (`zstd 0.45`, `gzip 0.50`, `xz 0.35`, `lz4 0.65`, `store 1.0`), e il planner viene **ricalcolato al volo**: se il flusso reale supera `LayerCount * LayerBytes`, gli ultimi layer crescono, non se ne aggiungono (l'implementazione della fase 04 deve rispettarlo, ed è testata lì).

### Test richiesti (tabella obbligatoria)

| Input | Attesa |
|---|---|
| 0 B | 1 layer |
| 100 MiB, target 1 GiB | 1 layer da 100 MiB (arrotondato a MinLayerBytes se serve) |
| 50 GiB, target 1 GiB | 50 layer da 1 GiB, nessun warning |
| 500 GiB, target 1 GiB | **118** layer, `LayerBytes ≈ 4,24 GiB`, 1 warning |
| 2 TiB, target 1 GiB | 118 layer, `LayerBytes ≈ 17,4 GiB`, 2 warning (limite + registry) |
| 10 MiB, target 1 GiB | `LayerBytes = MinLayerBytes` |
| `MaxDataLayers = 1` | sempre 1 layer, qualunque dimensione |

Più un test **invariante**: per 200 valori casuali di dimensione fra 1 KiB e 100 TiB, vale sempre `LayerCount <= MaxDataLayers` e `LayerCount * LayerBytes >= estimatedStoredBytes`.

### Definition of Done
- [ ] tutti i casi della tabella verdi
- [ ] il test invariante su 200 valori casuali verde
- [ ] copertura `pkg/chunk` ≥ 90 %

---

## 02.5 Benchmark e documentazione comparativa

**Agente: Haiku** (esecuzione e scrittura), **Sonnet** (codice del benchmark)

### File: `pkg/compress/bench_test.go`

Benchmark su tre corpora generati in modo riproducibile (seed fisso):
- **testo**: 256 MiB di log sintetici;
- **binario comprimibile**: 256 MiB di dati con entropia media (mix di zeri e pattern);
- **incomprimibile**: 256 MiB da `crypto/rand`.

Misurare per ogni codec e livello {min, default, max}: MB/s in compressione, MB/s in decompressione, rapporto.

### File: `docs/compression.md`

Tabella dei risultati sulla macchina di riferimento (indicare CPU e core), più:
- raccomandazione: **zstd default**;
- avvertenza esplicita che xz e lz4 rendono l'immagine non `docker run`-abile (rimando a D02 e al flag `--runnable`);
- guida alla scelta: "dati già compressi → `none` o `lz4`; archivio a lungo termine su banda scarsa → `xz`; tutto il resto → `zstd`".

### Definition of Done
- [ ] `go test -bench=. -benchtime=1x ./pkg/compress/` completa in < 10 minuti
- [ ] `docs/compression.md` contiene numeri reali, non segnaposto

---

## Gate di fase 02

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/compress/... ./pkg/chunk/...` | ≥ 90 % |
| **GS-02.1** | round-trip tabellare 5 codec × 5 input | tutti byte-identici |
| **GS-02.2** | tabella del planner (7 casi) | tutti conformi |
| **GS-02.3** | invariante del planner su 200 valori casuali | sempre `LayerCount <= 118` |
| **GS-02.4** | `AllocsPerRun` dello splitter | ≤ 1 |
| **GS-02.5** | fuzz zstd+gzip 60 s | zero crash |
| G9 | `make deps-check` | i 3 nuovi moduli sono in `docs/DEPENDENCIES.md` |
| G10 | `docs/compression.md` | presente, con numeri misurati |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**: `docs/compression.md`; aggiornamento di `docs/ARCHITECTURE.md` con la sezione "pianificazione dei layer" e l'algoritmo di 02.4.

**Rischio noto**: `ulikunitz/xz` è significativamente più lento delle alternative in decompressione. Se il benchmark mostra tempi inaccettabili (< 20 MB/s in decompressione), segnalare a Opus: si valuterà di declassare xz a "sconsigliato" nella documentazione, non di rimuoverlo.
