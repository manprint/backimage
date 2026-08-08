# Fase 11 — `repo`: elenco, cancellazione, retention, adapter per vendor

**Obiettivo**: gestire il ciclo di vita dei backup su un registry. È la parte che l'utente aveva ipotizzato ("sottocomando repository? listare i tag, fare retention, cancellare, editare un tag").

**Realtà da tenere presente**: la cancellazione non è uniforme. L'API OCI standard prevede `DELETE /v2/<name>/manifests/<digest>`, ma molti registry la disabilitano o la sostituiscono con API proprietarie. Per questo serve un'astrazione con **capability dichiarate**, e comandi che dicano chiaramente cosa possono e non possono fare su un dato registry.

---

## 11.1 Interfaccia `RegistryAdapter`

**Agente: Sonnet**

### File: `pkg/registry/adapter.go`

```go
// Capability flags what an adapter can do on a given registry.
type Capability uint32

const (
	CapListTags       Capability = 1 << iota // GET /v2/<name>/tags/list
	CapListRepos                             // catalog or vendor API
	CapDeleteManifest                        // DELETE by digest
	CapDeleteTag                             // delete a tag without deleting the manifest
	CapGarbageCollect                        // trigger blob GC
	CapUsageStats                            // report stored bytes
)

// Adapter talks to one registry vendor.
type Adapter interface {
	// Name identifies the adapter: "oci", "dockerhub", "ghcr", "ecr", "quay".
	Name() string
	// Capabilities reports what this adapter supports on this registry.
	Capabilities(ctx context.Context) (Capability, error)
	// ListTags returns the tags of a repository, newest first when the registry
	// exposes ordering, lexicographic otherwise.
	ListTags(ctx context.Context, repo name.Repository) ([]TagInfo, error)
	// DeleteTag removes one tag.
	DeleteTag(ctx context.Context, ref name.Tag) error
	// DeleteManifest removes a manifest by digest.
	DeleteManifest(ctx context.Context, ref name.Digest) error
	// Usage reports stored bytes for a repository, when supported.
	Usage(ctx context.Context, repo name.Repository) (int64, error)
}

// TagInfo describes one tag, enriched with backimage metadata when present.
type TagInfo struct {
	Tag       string
	Digest    v1.Hash
	Created   time.Time // from the image config or from backimage annotations
	Size      int64     // sum of layer sizes, shared blobs counted once per tag
	Backimage *index.Manifest // nil when the tag is not a backimage backup
}

// AdapterFor picks the adapter for a registry host, falling back to "oci".
func AdapterFor(host string, kc Keychain) (Adapter, error)

// RegisterAdapter makes an adapter available for a host suffix.
func RegisterAdapter(hostSuffix string, f func(Keychain) Adapter)
```

### Prescrizioni
- **Capability rilevate, non assunte**: `Capabilities` deve provare (es. un `DELETE` su un digest inesistente distingue 404 da 405/501) e memorizzare il risultato per la durata del processo.
- Un comando che richiede una capability assente fallisce con un messaggio che nomina il registry e spiega l'alternativa (es. *"Docker Hub non supporta la cancellazione via API OCI: usa l'interfaccia web oppure un token con l'API Hub v2"*).
- `TagInfo.Backimage` si popola leggendo le annotazioni del manifest: **non** scaricare il layer dei metadati per riempire un elenco di tag.

### Test
- adapter finto con capability variabili;
- rilevamento: registry che risponde 405 al DELETE → `CapDeleteManifest` assente;
- `AdapterFor("ghcr.io")` restituisce l'adapter ghcr; host sconosciuto → `oci`.

---

## 11.2 Adapter OCI generico

**Agente: Sonnet**

### File: `pkg/registry/adapter_oci.go`

- `ListTags`: `GET /v2/<name>/tags/list`, con paginazione via header `Link`.
- `DeleteManifest`: `DELETE /v2/<name>/manifests/<digest>`.
- `DeleteTag`: la maggior parte dei registry OCI non distingue: cancellare un tag significa cancellare il manifest a cui punta, il che rimuove anche gli altri tag che vi puntano. Prescrizione: `DeleteTag` risolve il tag in digest, verifica **quanti** tag puntano a quel digest e, se sono più di uno, fallisce con un errore che li elenca (a meno di `--force`). Comportamento sicuro per default.
- `Usage`: somma delle dimensioni dei blob unici; su registry senza catalogo, sommare per repository.
- `CapGarbageCollect`: assente per l'adapter generico (la GC di `registry:2` è un comando lato server, non un'API).

### Test contro `registry:2`
- elenco dei tag con paginazione (creare 150 tag);
- cancellazione di un manifest e verifica del 404 successivo;
- `DeleteTag` su un digest con 2 tag → errore che li elenca; con `--force` → procede.

---

## 11.3 Adapter per vendor

**Agente: Sonnet** (uno per sotto-attività), **Haiku** (raccolta della documentazione delle API)

| Adapter | File | Note |
|---|---|---|
| GHCR | `adapter_ghcr.go` | l'API OCI supporta il DELETE con un token `packages:delete`; l'elenco dei tag passa dall'API GitHub Packages quando è disponibile un PAT |
| Docker Hub | `adapter_dockerhub.go` | l'API OCI **non** permette il DELETE; usare `https://hub.docker.com/v2/repositories/<ns>/<repo>/tags/<tag>/` con JWT ottenuto da `/v2/users/login` |
| ECR | `adapter_ecr.go` | API AWS `BatchDeleteImage`; **non** aggiungere l'SDK AWS: usare l'API HTTP firmata con SigV4 implementata a mano **oppure**, se troppo oneroso, dichiarare `CapDeleteManifest` assente e rimandare alle policy di lifecycle di ECR. Decisione da prendere con Opus dopo aver misurato il costo. |
| Quay | `adapter_quay.go` | API `/api/v1/repository/<ns>/<repo>/tag/<tag>` con token OAuth |

### Prescrizioni
- Ogni adapter che richiede credenziali diverse da quelle del registry (PAT GitHub, JWT Hub, token Quay) le prende da `--api-token` / `BACKIMAGE_<VENDOR>_TOKEN`, **mai** riusando silenziosamente le credenziali di push.
- Ogni adapter ha un test con un server HTTP finto che riproduce le risposte reali (registrate in `testdata/`), non test che chiamano il servizio vero.
- Se un adapter non è implementabile senza dipendenze nuove, dichiarare le capability mancanti e documentarlo: **meglio un limite dichiarato che una dipendenza non concordata**.

### Test
- per ogni adapter: elenco tag, cancellazione, gestione dell'errore di autenticazione, gestione del rate limit (429 con `Retry-After`).

---

## 11.4 Comandi `repo`

**Agente: Sonnet**

```
backimage repo ls <REGISTRY>                 elenca i repository (se supportato)
backimage repo tags <REPO> [flags]           elenca i tag con metadati backimage
backimage repo rm <REPO:TAG|@DIGEST> [--force]
backimage repo prune <REPO> --keep-… [--dry-run]
backimage repo stats <REPO>                  spazio occupato, blob condivisi
backimage repo caps <REGISTRY>               mostra le capability rilevate
```

### `repo tags` — output

```
TAG                        CREATO             FILE     ORIGINALE   REGISTRY   CIFRATO
daily-20260808T183412Z     2026-08-08 18:34   12 843    47,2 GiB    20,0 GiB   sì
daily-20260807T183355Z     2026-08-07 18:33   12 801    47,1 GiB     2,1 GiB*  sì
weekly-20260803T020000Z    2026-08-03 02:00   12 340    46,8 GiB    19,7 GiB   sì

* dimensione incrementale: blob condivisi con altri tag contati una sola volta
```

Flag: `--json`, `--sort created|size|tag`, `--limit N`, `--all` (include i tag non backimage).

### Prescrizioni
- `repo rm` chiede **conferma interattiva** con il nome del tag da digitare, salvo `--yes`. È un'operazione distruttiva e irreversibile.
- `repo rm` di un tag che è l'unico riferimento a blob condivisi con altri tag: mostrare quanto spazio verrà **effettivamente** liberato (spesso molto meno del previsto con la dedup attiva).
- Con `--json` la conferma non è possibile: richiedere `--yes` esplicito, altrimenti errore.

---

## 11.5 Motore di retention

**Agente: Sonnet**

### File: `pkg/registry/retention.go`

```go
// Policy describes which tags to keep.
type Policy struct {
	KeepLast    int  // most recent N
	KeepHourly  int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int
	KeepWithin  time.Duration // keep everything newer than this
	KeepTags    []string      // glob patterns always kept
}

// Apply partitions tags into keep and remove sets. It never removes a tag that
// matches KeepTags, and it is deterministic.
func (p Policy) Apply(tags []TagInfo, now time.Time) (keep, remove []TagInfo)
```

### Prescrizioni
- Semantica identica a quella di restic/borg, che gli utenti già conoscono: un tag conservato da **una qualsiasi** regola è conservato.
- `Apply` è una funzione **pura**: nessuna rete, nessun orologio interno (`now` è un parametro). Questo la rende testabile in modo esaustivo.
- Se la policy è vuota, `remove` è vuoto: mai cancellare per default.
- `prune` senza `--dry-run` stampa comunque il piano e chiede conferma, salvo `--yes`.
- I tag senza data (non backimage) non vengono mai rimossi, salvo `--all`.

### Test (tabella obbligatoria)
- 100 tag giornalieri, `--keep-daily 7` → ne restano 7, i più recenti;
- `--keep-last 3 --keep-monthly 6` → unione, non intersezione;
- `--keep-within 30d` → tutto ciò che è più recente resta, anche oltre le altre regole;
- `--keep-tags "release-*"` → i tag corrispondenti restano sempre;
- policy vuota → `remove` vuoto;
- tag con la stessa data → ordinamento deterministico (per digest a parità di data);
- proprietà: `len(keep)+len(remove) == len(tags)` e i due insiemi sono disgiunti, su 500 casi generati casualmente.

---

## 11.6 End-to-end

**Agente: Sonnet**

### File: `test/e2e/phase_11.sh`

```
1.  registry:2; crea 30 backup con date simulate (--timestamp con date crescenti)
2.  backimage repo tags → 30 righe, ordinate
3.  backimage repo caps localhost:5000 → CapDeleteManifest presente
4.  backimage repo prune --keep-last 5 --dry-run → elenca 25 rimozioni, non cancella
5.  backimage repo prune --keep-last 5 --yes → restano 5 tag
6.  backimage restore del tag più vecchio rimasto → ZERO differenze
    ← dimostra che la prune non ha rotto i backup superstiti
7.  backimage repo stats → spazio coerente
8.  registry con DELETE disabilitato (registry:2 senza REGISTRY_STORAGE_DELETE_ENABLED)
    → repo rm fallisce con un messaggio che spiega come abilitarlo
```

Il passo 6 è il più importante: con la dedup attiva, cancellare un tag potrebbe rimuovere blob usati da altri. L'API OCI cancella i manifest, non i blob, e la GC è un'operazione separata lato server — ma il test deve dimostrarlo, non darlo per scontato.

### Definition of Done
- [ ] `make e2e PHASE=11` esce 0
- [ ] il passo 6 dà zero differenze

---

## Gate di fase 11

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/registry/...` | ≥ 85 % |
| G8 | `make e2e PHASE=11` | exit 0 |
| **GS-11.1** | proprietà della retention su 500 casi casuali | insiemi disgiunti e completi |
| **GS-11.2** | restore dopo prune | zero differenze |
| **GS-11.3** | `repo rm` senza `--yes` in modalità non interattiva | errore, nessuna cancellazione |
| **GS-11.4** | registry senza DELETE | messaggio esplicativo, exit ≠ 0, nessuna cancellazione parziale |
| **GS-11.5** | ogni adapter con server finto | test verdi, nessuna chiamata a servizi reali in CI |
| **GS-11.6** | policy vuota | zero rimozioni |
| G9 | `make deps-check` | nessuna dipendenza nuova non concordata (in particolare nessun SDK AWS) |
| G10 | `docs/retention.md`, aggiornamento di `docs/registries.md` | presenti |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**
- `docs/retention.md`: semantica delle regole con esempi, esempi di cron/systemd timer, avvertenza che la cancellazione dei manifest non libera lo spazio finché il registry non esegue la garbage collection.
- `docs/registries.md`: aggiornare con una tabella capability × vendor, e per ciascuno le istruzioni per ottenere il token dell'API di gestione.

**Rischi noti**
- ECR con SigV4 implementato a mano è la parte a costo più incerto. Se supera un giorno di lavoro, la scelta corretta è dichiarare la capability assente e rimandare alle lifecycle policy native di ECR: è una limitazione onesta e documentata, non un fallimento.
- La cancellazione è irreversibile e tocca i dati dell'utente: le conferme interattive e il `--dry-run` non sono opzionali e non vanno semplificati.
