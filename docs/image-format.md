# Formato immagine backimage (fase 04)

Un backup backimage viene prodotto come **immagine OCI multi-arch**: un
manifest index (OCI index) con un manifest per piattaforma, ognuno con
layer ordinati in modo stabile. L'immagine è **eseguibile**: `docker run`
avvia `/backimage` che legge i metadati in `/backup` e restaura il backup.

Schema di riferimento: `pkg/ociimg/build.go` (`BuildImage`, `BuildIndex`).

## Struttura dei layer

Ogni manifest di piattaforma ha **2 + N** layer:

| Layer | Path | Note |
|-------|------|------|
| 0 | `/backimage` | eseguibile self-extract, mode 0755, codec `store` (tar raw deterministico) |
| 1 | `/backup/...` | metadati, mode 0644, codec `store`: `manifest.json`, `chunks.json`, `index.json.zst`, `private.json.zst` e chiavi `keys*.age` (se cifrato) |
| 2..N | `/backup/data/...` | blob dati, condivisi tra piattaforme, codec da `--compression` |

L'ordine e i path sono **contratto**, garantito da `TestBuildImageLayerLayout` in `pkg/ociimg/build_test.go`.

- `/backimage` e `/backup` devono essere riproducibili → codec `store`
  (niente compressione, stessi digest a ogni rebuild).
- I layer dati devono essere **identici su tutte le piattaforme**:
  `BuildIndex` lo verifica e rifiuta con
  `data layers must be identical across platforms`.

## Config (`config.json`)

- `Architecture` / `OS` dalla piattaforma (`linux/amd64`, ...).
- `Entrypoint = ["/backimage"]`, `Cmd = ["info"]`: `docker run` lancia il
  self-extract. `WorkingDir = "/"`, `User = "0:0"` (serve per ripristinare
  permessi e device).
- `Env` vuoto.
- `Labels`: lo stesso set è scritto sia nel config sia nelle **annotazioni
  del manifest** (visibile con `docker inspect` e `crane manifest`).

### Etichette e annotazione

| Label | Valore | Note |
|-------|--------|------|
| `org.opencontainers.image.created` | RFC3339 UTC | riproducibile se passata, altrimenti wall clock |
| `org.opencontainers.image.title` | `backimage backup` | |
| `org.opencontainers.image.description` | `run this image to restore the backup` | |
| `dev.backimage.schema-version` | `1` (non cifrato) o `2` (cifrato) | |
| `dev.backimage.tool-version` | versione binario | |
| `dev.backimage.encrypted` | `true` / `false` | |
| `dev.backimage.compression` | `store`, `gzip`, `zstd`, `xz`, `lz4` | |
| `dev.backimage.chunks` | `manifest.chunking.count` | |
| `dev.backimage.files` | `manifest.totals.files` | **omessa** se il backup è cifrato |
| `dev.backimage.bytes-raw` | `manifest.totals.bytesRaw` | **omessa** se il backup è cifrato |
| `dev.backimage.sources` | sorgenti originale `;`-separate | **omessa** se il backup è cifrato o con `--no-metadata` |

Le label sono leggibili dal registry senza scaricare l'immagine: un backup
cifrato non ne pubblica nessuna che descriva il contenuto.

### Meta-dati in `/backup`

Lo `schemaVersion` dei metadati vale **1** per un backup non cifrato (dove
tutto è pubblico per definizione) e **2** per un backup cifrato, dove i campi
riservati stanno nel blob privato:

- `manifest.json`: `index.Manifest` — mai cifrato, contiene `layers[]`
  (digest, intervallo chunk), `index` (ref al blob indice), formato, codec e
  le impostazioni di cifratura necessarie prima dello sblocco (`aead`,
  `nonceMode`, `envelopeVersion`). In schema 2 contiene anche `private` (ref al
  blob privato) e **non** contiene `sources`, `host`, `totals`,
  `encryption.keyFingerprint` né `encryption.recipients`.
  `encryption.envelopeVersion` è pubblico per necessità: un backup `--dedup`
  successivo deve poter decidere, prima di aprire qualsiasi cosa, se la chiave
  che sta per riusare ha mai sigillato con la derivazione nonce precedente alla
  0.2.4 (vedi [security.md](security.md)). Assente significa envelope v1.
- `chunks.json`: `index.ChunkTable` — chunk→blob: `i`, path, sha e byte del
  blob **memorizzato** (`ss`, `sb`), che servono a localizzare e verificare i
  chunk senza chiave. In schema 2 `ps` e `pb` (sha e byte del **plaintext**)
  sono assenti: stanno nel blob privato.
- `index.json.zst`: blob indice (elenco file), **già compresso** (zstd) e, se
  cifrato, **avvolto nell'envelope crypt** (vedi `docs/security.md`).
- `private.json.zst`: solo per backup cifrati — JSON zstd **sempre**
  nell'envelope crypt, con `sources`, `host`, `totals`, impronta e recipient
  della chiave e la coppia `ps`/`pb` di ogni chunk. Dopo lo sblocco
  `pkg/recovery` lo fonde in memoria nel manifest e nella chunk table, così i
  lettori a valle vedono la forma di sempre.
- `keys.age` etc.: file chiavi (solo se cifrato).

Un backimage che legge un'immagine di schema 1 la restaura come prima; un
backimage precedente allo schema 2 rifiuta un'immagine nuova con
`backup creato da un backimage più recente`.

Da 0.2.4 i blob usano l'envelope `BIMGCHK1` **versione 2** (stesso layout di
byte, nonce convergente derivato dal payload sigillato e ruolo del blob nei dati
autenticati). La versione 1 continua a essere letta, quindi le immagini già
pubblicate si ripristinano intatte; un `backimage` precedente alla 0.2.4 rifiuta
un blob nuovo con `unsupported blob version 2 (support 1-2)`.

## Media type dei layer

Il media type OCI dipende dal codec (`pkg/ociimg/layer.go`):

- `store` → `application/vnd.oci.image.layer.v1.tar`
  (⚠ ggcr: `types.OCILayer` = tar+gzip; per `store` usare
  `types.OCIUncompressedLayer`).
- `gzip` → `.tar+gzip` (`types.OCILayer`)
- `zstd` → `.tar+zstd` (`types.OCILayerZStd`)
- `xz`, `lz4` → nessuna costante in ggcr: **media type non standard**.
  Per immagini eseguibili vengono rifiutati (guard `errNonStandardCodec`):
  usare `--compression zstd|gzip` oppure `--runnable` di pull-only.

## Consistenza multi-piattaforma

`BuildIndex` (`pkg/ociimg/build.go`):
1. verifica che il layer meta (indice 1) e i layer dati (2..N) abbiano
   **digest uguali** tra piattaforme;
2. ordina i manifest per OS/arch;
3. appende `v1.Descriptor{MediaType: OCIManifestSchema1, platform}`.

## Ispezione

```sh
docker manifest inspect IMG
docker inspect IMG --format '{{json .Config.Labels}}'
docker pull --platform linux/amd64 IMG

crane manifest IMG | jq .layers[].mediaType
crane export IMG | tar tvf -      # file system completo

skopeo inspect docker://IMG --raw | jq
```

## Compatibilità registry (e2e `test/e2e/phase_04.sh`)

- `registry:2`: il manifest list salvato è un **OCI index**; le richieste
  v2 API devono includere `Accept: application/vnd.oci.image.index.v1+json`
  (altrimenti 404 `MANIFEST_UNKNOWN`).
- Target `daemon`: `pkg/v1/daemon.Write` passa per un **tarball
  docker-save temporaneo** → occupa circa quanto l'immagine; non adatto a
  backup enormi. I digest dei layer nel tarball differiscono (wrap
  docker-save) → i test confrontano contenuti e ConfigName, non digest.
- Registry restrittivi (es. ECR con policy severe) possono rifiutare
  `artifactType` o annotazioni non note: sezione compatibilità da
  approfondire in fase 05.

## Limiti noti (roadmap)

- `--local-repo` e `--output` sono in conflitto (`KindUsage`): rilevato da
  eseguire nella fase 05 (CLI).
- `daemon.Write` reale con la v0.21.9 di go-containerregistry può fare
  panic con certe immagini (nil deref): nei test si inietta un mock
  (`var daemonWrite`); da valutare l'aggiornamento della libreria.