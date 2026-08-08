# Fase 04 — `pkg/index` e `pkg/ociimg`: assemblaggio dell'immagine

**Obiettivo**: dai chunk cifrati della fase 03 all'immagine OCI **multi-arch, pullabile ed eseguibile**, con entrypoint, annotazioni e i quattro formati di output.

**Riferimento decisioni**: D01, D02, D03, D04, D05.

---

## 04.1 Modelli `Manifest`, `Chunks`, `Index`

**Agente: Sonnet**

### File: `pkg/index/model.go`

Implementare **esattamente** le strutture JSON di `overview.md` §4.1–4.3, con tag JSON identici a quelli documentati (i nomi brevi `i/p/ps/ss/pb/sb` in `chunks.json` sono voluti: con 12 800 chunk fanno la differenza).

```go
// SchemaVersion is the version written into every metadata file.
const SchemaVersion = 1

// Manifest is the public, unencrypted description of a backup.
type Manifest struct { … }

// ChunkTable maps chunks to blobs. It can be large and is written separately.
type ChunkTable struct { … }

// Index is the per-file table. It is encrypted when encryption is enabled.
type Index struct { … }

// FileEntry describes one archived filesystem object.
type FileEntry struct { … }
```

Costruttori e I/O:

```go
// WriteManifest serialises m as indented JSON.
func WriteManifest(w io.Writer, m *Manifest) error
// ReadManifest parses and validates a manifest.
func ReadManifest(r io.Reader) (*Manifest, error)

// WriteChunkTable serialises t as compact JSON (no indentation).
func WriteChunkTable(w io.Writer, t *ChunkTable) error
func ReadChunkTable(r io.Reader) (*ChunkTable, error)

// WriteIndex serialises, compresses with zstd and optionally encrypts the index.
func WriteIndex(w io.Writer, idx *Index, sealer crypt.Sealer) error
func ReadIndex(r io.Reader, opener crypt.Opener) (*Index, error)
```

### Prescrizioni
- Ogni `Read*` **valida**: `schemaVersion` supportata, campi obbligatori presenti, coerenza fra `chunking.count` e `len(chunks)`. Uno schema futuro sconosciuto produce l'errore *"backup creato con backimage più recente: aggiornare (schema %d, supportato %d)"*.
- `Index` può contenere milioni di voci: `ReadIndex` deve usare `json.Decoder` in streaming sull'array `entries`, non caricare tutto in una `[]byte` prima del parse. Test con 1 000 000 di voci sintetiche entro 512 MB di RSS.
- `FileEntry.Mode` è una **stringa ottale** (`"0644"`, `"04755"`): resiste alle differenze di rappresentazione fra piattaforme e resta leggibile.

### Test richiesti
- round-trip di ciascuna struttura;
- golden file in `testdata/` per tutti e tre i formati, con test di stabilità del JSON prodotto (chiavi ordinate, nessuna differenza fra due serializzazioni);
- schema futuro (`schemaVersion: 99`) → errore con il messaggio prescritto;
- index da 1 000 000 di voci: parse in streaming, memoria sotto controllo (`runtime.ReadMemStats`).

---

## 04.2 Mappa offset→chunk e ricerca per path

**Agente: Sonnet**

### File: `pkg/index/lookup.go`

```go
// Locator answers "which chunks do I need" questions.
type Locator struct{ … }

// NewLocator builds a locator from a chunk table.
func NewLocator(t *ChunkTable) *Locator

// ChunkForOffset returns the index of the chunk containing the given offset in
// the plaintext tar stream, and the offset within that chunk.
func (l *Locator) ChunkForOffset(off int64) (chunkIndex int, inner int64, err error)

// Range returns the inclusive chunk index range covering [start,end) of the
// plaintext stream.
func (l *Locator) Range(start, end int64) (from, to int, err error)

// TotalPlainBytes returns the size of the plaintext tar stream.
func (l *Locator) TotalPlainBytes() int64

// EntriesMatching filters index entries with include/exclude glob patterns.
func EntriesMatching(idx *Index, includes, excludes []string) ([]FileEntry, error)

// ChunksFor returns the sorted, deduplicated set of chunk indices needed to
// extract the given entries.
func ChunksFor(l *Locator, entries []FileEntry) ([]int, error)
```

### Prescrizioni
- `ChunkForOffset` con ricerca binaria su offset cumulativi precalcolati in `NewLocator` (`[]int64` di prefissi). Nessuna scansione lineare.
- `ChunksFor` deve considerare che una entry occupa `header (512B allineato) + dati (arrotondati a 512)`: calcolare `end = tarOffset + 512 + roundUp512(size)`.
- Glob: `path.Match` su path con `/`, più la regola pratica che un pattern che termina con `/` o che corrisponde a una directory include ricorsivamente il suo contenuto.

### Test richiesti
- tabella `ChunkForOffset` su una tabella di 10 chunk di dimensione irregolare, inclusi gli estremi (offset 0, ultimo byte, oltre la fine → errore);
- `Range` che attraversa 1, 2 e 5 chunk;
- `EntriesMatching` con `--include "myfiles/docs/**"` e `--exclude "**/*.tmp"`;
- restore parziale di **un** file da un backup di 1000 file richiede meno di 3 chunk (test dell'efficacia).

---

## 04.3 Costruttore di layer tar deterministici

**Agente: Sonnet** — *prerequisito della dedup di fase 10*

### File: `pkg/ociimg/layer.go`

```go
// LayerFile is one file to be placed inside a layer tar.
type LayerFile struct {
	Path    string // absolute path inside the image, e.g. "/backup/data/000000.blob"
	Mode    int64  // 0644 or 0755
	Size    int64
	Open    func() (io.ReadCloser, error)
}

// BuildLayerTar writes a deterministic tar containing files, in the order given.
// Ownership is forced to 0:0, modification time to the Unix epoch, and no PAX
// records are emitted: two calls with identical inputs produce identical bytes.
func BuildLayerTar(w io.Writer, files []LayerFile) error

// NewLayer returns a ggcr layer for the given files, compressed with codec.
func NewLayer(files []LayerFile, codec compress.Codec, level int) (v1.Layer, error)
```

### Prescrizioni di determinismo (vincolanti)
- `ModTime`, `AccessTime`, `ChangeTime` = `time.Unix(0,0).UTC()`;
- `Uid=0, Gid=0, Uname="", Gname=""`;
- `Format = tar.FormatUSTAR` quando i path lo permettono (sono tutti brevi e ASCII per costruzione), altrimenti `FormatPAX`;
- nessun `PAXRecords`;
- le directory intermedie (`/backup`, `/backup/data`) sono emesse **esplicitamente** e una sola volta, con mode `0755`, in ordine lessicografico prima dei file che contengono;
- il livello di compressione è fissato dal chiamante e registrato nel manifest: cambiarlo cambia i digest, ed è atteso.

### Test richiesti
- **due invocazioni identiche → digest SHA-256 identico** (test cardine);
- il tar prodotto è leggibile da `tar tvf` e da `archive/tar`;
- ordine delle voci uguale all'ordine di `files`;
- un layer da 118 file con path a 6 cifre resta in USTAR.

---

## 04.4 Assemblaggio dell'immagine ggcr

**Agente: Sonnet**

### File: `pkg/ociimg/build.go`

```go
// BuildOptions describes one platform image.
type BuildOptions struct {
	Platform     v1.Platform      // linux/amd64 or linux/arm64
	SelfExtract  []byte           // static binary placed at /backimage
	Manifest     *index.Manifest
	ChunkTable   *index.ChunkTable
	IndexBlob    []byte           // already compressed and possibly encrypted
	KeyFiles     map[string][]byte// "backup/keys.age" -> bytes; empty when --no-encrypt
	DataLayers   []v1.Layer       // shared across platforms
	Codec        compress.Codec
	Level        int
	Annotations  map[string]string
}

// BuildImage assembles a single-platform image.
func BuildImage(opts BuildOptions) (v1.Image, error)

// BuildIndex assembles a multi-platform image index from per-platform images
// that share the same data layers.
func BuildIndex(images map[v1.Platform]v1.Image, annotations map[string]string) (v1.ImageIndex, error)
```

### Ordine dei layer (vincolante)

| # | Contenuto | Condiviso fra piattaforme |
|---|---|---|
| 0 | `/backimage` | **no** (uno per architettura) |
| 1 | `/backup/manifest.json`, `/backup/chunks.json`, `/backup/index.json.zst[.age]`, `/backup/keys*.age` | sì |
| 2…N | `/backup/data/NNNNNN.blob` | sì |

### Config OCI da produrre

```go
cfg.Architecture = platform.Architecture
cfg.OS           = "linux"
cfg.Config.Entrypoint = []string{"/backimage"}
cfg.Config.Cmd        = []string{"info"}
cfg.Config.WorkingDir = "/"
cfg.Config.User       = "0:0"
cfg.Config.Env        = nil            // niente PATH: non c'è shell
cfg.Config.Labels     = <vedi sotto>
```

### Label e annotazioni

| Chiave | Valore |
|---|---|
| `org.opencontainers.image.created` | RFC3339 della creazione |
| `org.opencontainers.image.title` | `backimage backup` |
| `org.opencontainers.image.description` | `run this image to restore the backup` |
| `dev.backimage.schema-version` | `1` |
| `dev.backimage.tool-version` | versione |
| `dev.backimage.encrypted` | `true`/`false` |
| `dev.backimage.compression` | nome del codec |
| `dev.backimage.files` | conteggio |
| `dev.backimage.bytes-raw` | dimensione originale |
| `dev.backimage.chunks` | conteggio |
| `dev.backimage.sources` | path sorgente, **omesso se `--no-metadata`** |

Le stesse coppie vanno sia in `cfg.Config.Labels` sia nelle annotazioni del manifest, così `docker inspect` e `crane manifest` mostrano entrambi l'informazione.

### Prescrizioni
- **Guardia D02**: se `Codec.MediaTypeSuffix() == ""` e `opts.Runnable` (da flag) è vero → errore con l'hint prescritto nella fase 02.1.
- `SelfExtract` vuoto → errore: un'immagine senza entrypoint viola il requisito centrale.
- Il layer 0 va costruito con mode `0755` sul file `/backimage`.
- `BuildIndex` deve verificare che i digest dei layer di dati siano **identici** fra le piattaforme (altrimenti si caricherebbero due volte) e fallire se non lo sono.

### Test richiesti
- immagine costruita: `img.Layers()` ha `2 + len(DataLayers)` elementi;
- `img.ConfigFile()` ha entrypoint, cmd, user e label attesi;
- `BuildIndex` con due piattaforme: `index.IndexManifest().Manifests` ha 2 voci con `Platform` corretta;
- `BuildIndex` con layer di dati diversi fra piattaforme → errore;
- codec xz con `Runnable=true` → errore con hint;
- il digest dell'immagine è **riproducibile**: due `BuildImage` con gli stessi input danno lo stesso digest.

---

## 04.5 Output: registry, daemon, oci-layout, tar

**Agente: Sonnet**

### File: `pkg/ociimg/output.go`

```go
// Target names an output destination.
type Target string

const (
	TargetRegistry  Target = "registry"
	TargetDaemon    Target = "daemon"
	TargetOCILayout Target = "oci-layout"
	TargetTar       Target = "tar"
)

// Writer publishes an image index to one destination.
type Writer interface {
	// Write publishes idx under ref and reports progress on ch (may be nil).
	Write(ctx context.Context, ref name.Reference, idx v1.ImageIndex, ch chan<- Progress) error
	// Name returns the target name.
	Name() Target
}

// NewWriter returns the writer for t. path is used by oci-layout and tar.
func NewWriter(t Target, path string, opts WriterOptions) (Writer, error)

// Progress reports transferred bytes for one blob.
type Progress struct {
	Blob      v1.Hash
	Total     int64
	Completed int64
	Layer     int
	Skipped   bool // blob already present on the registry
}
```

### Prescrizioni
- `TargetRegistry`: `remote.WriteIndex` con `remote.WithAuth`, `remote.WithContext`, `remote.WithProgress`, `remote.WithJobs(n)`.
- `TargetDaemon`: `daemon.Write` funziona su **una** `v1.Image`, non su un index. Comportamento prescritto: selezionare il manifest per la piattaforma host (`runtime.GOARCH`), scriverlo, e avvisare che il daemon riceve solo quella piattaforma. Se `GOARCH` non è fra quelle costruite → errore con hint.
- `TargetOCILayout`: `layout.Write(path, idx)`.
- `TargetTar`: `tarball.WriteToFile` sulla singola immagine della piattaforma host, con la stessa avvertenza del daemon.
- Il flag utente è `--local-repo` (booleano, come richiesto dall'utente): equivale a `--output daemon`. La forma generale `--output` resta disponibile. Se entrambi sono presenti e in conflitto → errore `KindUsage`.

### Test richiesti
- `oci-layout` e `tar` su directory temporanea, poi rilettura con `layout.ImageIndexFromPath` / `tarball.ImageFromPath` e confronto dei digest;
- `registry` verso il registry in-memory di ggcr (`ggcr/pkg/registry`): push e ri-pull, digest identico;
- `daemon` con host `GOARCH` non costruito → errore con hint;
- conflitto `--local-repo` + `--output registry` → `KindUsage`.

---

## 04.6 Test end-to-end della fase

**Agente: Sonnet**

### File: `test/e2e/phase_04.sh`

1. avvia `registry:2` su `localhost:5000` (o usa quello del servizio CI);
2. costruisce un'immagine sintetica con 3 layer di dati finti e un `/backimage` finto (`#!/bin/sh` non serve: basta un file binario qualsiasi, non viene eseguito in questa fase);
3. push su `localhost:5000/test/img:v1`;
4. `docker pull localhost:5000/test/img:v1` → **deve riuscire**;
5. `docker inspect` → verifica entrypoint, cmd e le label `dev.backimage.*` con `jq`;
6. `docker manifest inspect` → verifica le due piattaforme;
7. verifica che i digest dei layer di dati siano gli stessi nei due manifest (conteggio dei blob unici sul registry: `GET /v2/test/img/manifests/...` e confronto);
8. `crane` non è richiesto: usare `bin/backimage` o `curl` sull'API v2.

### Definition of Done
- [ ] `make e2e PHASE=04` esce 0
- [ ] il punto 7 dimostra che i layer di dati sono caricati **una volta sola**

---

## Gate di fase 04

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/index/... ./pkg/ociimg/...` | ≥ 85 % |
| G8 | `make e2e PHASE=04` | exit 0 |
| **GS-04.1** | `BuildLayerTar` due volte | digest identico |
| **GS-04.2** | `BuildImage` due volte | digest immagine identico |
| **GS-04.3** | `docker pull` dell'immagine prodotta | exit 0 |
| **GS-04.4** | `docker inspect … \| jq -e '.[0].Config.Entrypoint[0]=="/backimage"'` | exit 0 |
| **GS-04.5** | blob di dati unici sul registry | uguali al numero di layer di dati, non al doppio |
| **GS-04.6** | index da 1 000 000 di voci | parse in streaming, RSS < 512 MB |
| **GS-04.7** | codec senza media type + `--runnable` | errore con hint |
| G9 | `make deps-check` | `go-containerregistry` documentato |
| G10 | `docs/image-format.md` | presente |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**: `docs/image-format.md` — layout dell'immagine, ordine dei layer, config, label e annotazioni, come ispezionarla con `docker`, `crane`, `skopeo`; aggiornamento di `docs/ARCHITECTURE.md`.

**Rischi noti**
- `daemon.Write` passa da un tarball temporaneo: su backup grandi consuma spazio quanto l'immagine. Documentarlo in `docs/image-format.md` e ricordare che `--local-repo` non è adatto ai backup enormi.
- Alcuni registry (ECR con impostazioni restrittive) rifiutano `artifactType` o manifest list con annotazioni non note: il test e2e usa `registry:2`, ma va aperta una sezione "compatibilità registry" in `docs/image-format.md` da riempire nella fase 05.
