# Architettura

## Layout dell'immagine prodotta

```
/backimage                      binario statico auto-estraente (ENTRYPOINT)  [layer per-arch]
/backup/manifest.json           metadati PUBBLICI, in chiaro                 [layer condiviso]
/backup/chunks.json             mappa chunk→layer/offset, in chiaro          [layer condiviso]
/backup/keys.age                DEK incapsulata (assente se --no-encrypt)    [layer condiviso]
/backup/index.json.zst[.age]    indice file (CIFRATO se cifratura attiva)    [layer condiviso]
/backup/data/000000.blob …      chunk compressi (+cifrati)                   [layer 2..N]
```

Config OCI:

```json
{
  "config": {
    "Entrypoint": ["/backimage"],
    "Cmd": ["info"],
    "WorkingDir": "/",
    "User": "0:0",
    "Labels": { "dev.backimage.schema-version": "1" }
  }
}
```

### Assemblaggio dell'immagine (fase 04, `pkg/ociimg`)

- Ordine dei layer per piattaforma (**contratto**): layer 0 = `/backimage`
  (0755), layer 1 = `/backup` (metadati, 0644), layer 2..N = blob dati.
  `/backimage` e `/backup` usano codec `store` (riproducibilità); i blob
  dati usano il codec scelto.
- Config: `Architecture`/`OS` dalla piattaforma; le etichette (vedi
  `docs/image-format.md`) sono scritte sia nel config sia nelle
  **annotazioni del manifest** (`mutate.Annotations`).
- `BuildImage` (singola piattaforma) + `BuildIndex` (multi-arch): il
  secondo verifica digest identici di meta+data tra piattaforme e ordina i
  manifest per OS/arch.
- Media type layer per codec: `store`=tar (ggcr `OCIUncompressedLayer`),
  `gzip`=tar+gzip, `zstd`=tar+zstd; `xz`/`lz4` non hanno costante ggcr →
  vietati per immagini eseguibili (guard `errNonStandardCodec`).
- Se `--no-metadata`: `manifest.sources` nil → label `dev.backimage.sources`
  omessa.
- Target output (04.5): `registry` (remote.WriteIndex), `daemon`
  (`pkg/v1/daemon.Write`, tarball docker-save temporaneo), `oci-layout`,
  `tar`. Scelta del layer per piattaforma ospite: `--platform` o
  runtime.goos/goarch, a errore se mancante.

### Comandi utente finale, senza backimage installato

```bash
docker run --rm IMG                                   # info pubbliche, nessuna password
docker run --rm -it IMG list                          # elenco file (chiede passphrase)
docker run --rm -i  IMG tar > backup.tar              # tar su stdout — FEDELTÀ TOTALE
docker run --rm -it -v "$PWD:/restore" IMG extract --out /restore
docker run --rm -it IMG verify
```

## Formati dati (schemaVersion 1)

### manifest.json (pubblico, piccolo)

```json
{ "schemaVersion": 1, "tool": {"name":"backimage","version":"0.1.0"},
  "createdAt": "…", "sources": ["/home/fabio/myfiles"],
  "host": {"hostname":"…","os":"linux","arch":"amd64"},
  "totals": {"files":0,"dirs":0,"symlinks":0,"hardlinks":0,"devices":0,"bytesRaw":0,"bytesStored":0},
  "archive": {"format":"tar-pax","compression":"zstd","compressionLevel":3},
  "encryption": {"enabled":true,"kdf":"scrypt","aead":"aes-256-gcm","nonceMode":"random","recipients":["scrypt"]},
  "chunking": {"strategy":"fixed","targetChunkBytes":4194304,"count":0,"polynomial":0},
  "layers": [ {"index":0,"digest":"sha256:…","chunkFrom":0,"chunkTo":63,"storedBytes":0} ],
  "index": {"path":"backup/index.json.zst.age","storedSha256":"…","encrypted":true} }
```

### chunks.json (pubblico, può essere grande)

```json
{ "schemaVersion":1,
  "chunks":[ {"i":0,"p":"backup/data/000000.blob","ps":"sha256 plaintext","ss":"sha256 stored","pb":4194304,"sb":1048576} ] }
```

`pb` = plain bytes, `sb` = stored bytes. La concatenazione dei plaintext dei
chunk in ordine di `i` è esattamente il flusso tar non compresso.

### index.json (cifrato se attivo)

```json
{ "schemaVersion":1,
  "entries":[ {"path":"myfiles/a.txt","type":"reg","size":123,"mode":"0644","uid":1000,"gid":1000,
               "uname":"fabio","gname":"fabio","mtime":"…","linkTarget":"","tarOffset":1536,"sha256":"…"} ] }
```

### Envelope di un blob

```
0   8   magic "BIMGCHK1"
8   1   version = 1
9   1   codec   0=store 1=gzip 2=zstd 3=xz 4=lz4
10  1   aead    0=none 1=aes-256-gcm
11  1   flags   bit0 = nonce convergente
12  12  nonce   (assente se aead=0)
24  …   payload (compresso, poi cifrato) + tag GCM 16B
```

Ordine invariabile: **tar → compressione → cifratura**. AAD del GCM =
`magic||version||codec||aead||flags||uint32be(chunkIndex)`.

### keys.age

File age (armored) con JSON: `{"schemaVersion":2,"envelopeVersion":3,`
`"nonceMode":"random|convergent","reuse":"never|convergent-dedup",`
`"dek":"<base64 32B>","nonceKey":"<base64 32B>"}`. I tre campi di attestazione
dicono per quale epoca crittografica la chiave è stata creata e se può
sigillare di nuovo: stanno dentro l'involucro age, quindi sono autenticati da
esso, e sono l'unica autorità sul riuso. `schemaVersion: 1` (fino alla 0.4.0)
non attesta nulla e non viene mai riusato.
Destinatari: `scrypt` (passphrase) e/o `age1…` X25519.
Se la cifratura è attiva, senza passphrase o chiave privata il backup è
**irrecuperabile**.

### private.json.zst

Solo per backup cifrati (`schemaVersion: 2`): blob nell'envelope crypt con i
metadati che descrivono il contenuto — `sources`, `host`, `totals`, impronta e
recipient della chiave, e per ogni chunk digest e byte del plaintext. Il
manifest pubblico lo referenzia in `private` e non contiene quei campi; dopo lo
sblocco `pkg/recovery` li rifonde in memoria.

Porta anche `binding`, il legame autenticato con gli altri file di metadati
(digest di manifest, chunk table e blob indice, più la politica attesa),
verificato subito dopo `Unlock` e prima di qualunque consegna di dati.
Dettagli in [image-format.md](image-format.md) e [security.md](security.md).

## Pianificazione dei layer (overlayfs 127)

Limite overlayfs: 127 layer → massimo **118 layer di dati** (1 binario + 1
metadati + margine). Se la stima della dimensione supera il target, si
**aumenta la dimensione del layer**, mai il numero. `ChunkBytes =
clamp(layerBytes/64, 1 MiB, 64 MiB)`.

Implementazione: `pkg/chunk.PlanLayers` (file `pkg/chunk/plan.go`), limiti di
default in `LayerLimits{ MaxDataLayers: 118, MaxLayerBytes: 5 GiB,
MinLayerBytes: 16 MiB, TargetLayerBytes: 1 GiB }`.

### Algoritmo (02.4, da implementare esattamente così)

1. `estimatedStoredBytes <= 0` → 1 layer, `LayerBytes = MinLayerBytes`, fine.
2. `layerBytes = TargetLayerBytes`.
3. `layerCount = ceil(estimated / layerBytes)`.
4. Se `layerCount > MaxDataLayers`: `layerCount = MaxDataLayers`,
   `layerBytes = ceil(estimated / layerCount)`, warning «backup grande:
   dimensione layer portata a X per restare entro N layer (limite overlayfs)».
5. Se `layerBytes < MinLayerBytes`: `layerBytes = MinLayerBytes`, ricalcolo di
   `layerCount` (≤ MaxDataLayers per costruzione).
6. Se `layerBytes > MaxLayerBytes`: warning «layer da X: alcuni registry
   rifiutano blob così grandi» (non è un errore).
7. `ChunkBytes = clamp(layerBytes / 64, 1 MiB, 64 MiB)`.

Un singolo layer si dimensiona sul dato effettivo (mai sotto MinLayerBytes):
100 MiB con target 1 GiB → layer da 100 MiB.

### Ricalcolo al volo (fase 04)

`estimatedStoredBytes` è una stima: la fase 05 la calcola come
`bytesRaw * fattore` per codec (zstd 0.45, gzip 0.50, xz 0.35, lz4 0.65,
store 1.0). Se il flusso reale supera `LayerCount * LayerBytes`, gli ultimi
layer **crescono**, non se ne aggiungono: il numero di layer è vincolato dal
gate overlayfs e non cambia durante lo streaming.
## Tetti del lettore e da dove derivano

I metadati pubblici di un backup — `manifest.json` e `chunks.json` — decidono
quanta memoria un lettore sta per allocare. Nessuno dei due porta una firma,
quindi ogni numero che arriva da lì è una **dichiarazione**, non un fatto.

La regola è che nessun tetto sia scelto a caso: ogni limite deriva da un
valore che il backup stesso dichiara, oppure da qualcosa che il lettore può
misurare, e la verifica precede l'allocazione.

### Tabella dei chunk (`index.ValidateChunkTable`)

Eseguita una volta sola, all'apertura del backup, prima che qualunque
percorso di lettura usi una dimensione:

| Vincolo | Derivato da |
| --- | --- |
| `count` del manifest = numero di righe | `manifest.chunking.count` |
| ogni riga è al proprio indice (`i` = posizione) | la tabella stessa |
| `sb > 0` e `p` non vuoto | — (una riga senza blob non è leggibile) |
| i layer coprono `[0, count)` in ordine e senza buchi | `manifest.layers[].chunkFrom/chunkTo` |
| tutti i chunk di un layer nominano lo stesso blob | il writer produce un blob per layer |
| due layer non leggono lo stesso blob | gli offset sono progressivi dentro un blob |
| `sb ≤ storedBytes` del proprio layer | `manifest.layers[].storedBytes` |
| somma degli `sb` di un layer = `storedBytes` | `manifest.layers[].storedBytes` |
| `sb ≤ maxChunkBytes` (o `targetChunkBytes`) più un margine | `manifest.chunking` |

Il margine è l'unico numero senza controparte nei file: `dimensione/64 + 4096
byte`, cioè assai più di quanto qualunque codec di questo progetto aggiunga a
un input incomprimibile, più l'header dell'envelope e il tag GCM. Serve a
rifiutare una dichiarazione assurda, non a indovinare il comportamento di un
compressore.

Ogni violazione è `index.ErrBadSchema`, che la CLI classifica come risposta di
integrità (exit code 5).

### La dimensione reale del blob, dove è disponibile

I vincoli sopra mettono d'accordo due file pubblici fra loro; se un attaccante
li riscrive **entrambi** restano coerenti. L'ultima autorità è allora il blob
sul disco:

- `pkg/recovery` interroga la sorgente con `Stat()` quando questa lo permette —
  un backup locale e l'immagine autoestraente restituiscono un `*os.File`,
  quindi è il caso comune e costa una `fstat`;
- `pkg/restore` misura il file di layer già materializzato prima di leggerne un
  chunk.

In entrambi i casi, `offset + sb` oltre la fine del blob è un errore di formato
prima di essere un'allocazione.

### Blob di metadati (`index.LimitMetadata`)

`manifest.json`, `chunks.json`, `index.json.zst` e `private.json.zst` venivano
letti con `io.ReadAll` su un reader che arriva dall'immagine: nessun tetto, e
il layer dei metadati è compresso, quindi i byte che il lettore avrebbe tenuto
in memoria li produce il decoder, non chi ha pubblicato l'immagine.

Ogni lettura passa ora da un tetto:

| Sorgente | Tetto |
| --- | --- |
| backup locale, immagine autoestraente | la dimensione reale del file (`fstat`) |
| layer dei metadati di un'immagine OCI | `DefaultMaxMetadataBytes`, come budget dell'intero layer e non della singola entry |
| qualunque altro reader | `DefaultMaxMetadataBytes` |

`DefaultMaxMetadataBytes` è **512 MiB** ed è l'unico tetto assoluto del
lettore. La derivazione è la forma dei due blob che crescono col backup:
l'indice ha una riga per file archiviato, i metadati riservati una riga per
chunk, una riga JSON è dell'ordine dei duecento byte e si comprime più volte.
512 MiB *memorizzati* sono quindi decine di milioni di file o di chunk, molto
oltre quel che questo strumento può scrivere restando nei propri limiti di
layer.

Un tetto più stretto passato da chi sa misurare il blob vince; uno più largo
no, perché i numeri che un chiamante potrebbe passare vengono dall'immagine.

### Decompressione (`compress.MaxDecoderMemory` e il tetto per chunk)

Due limiti distinti, perché sono due cose distinte.

**Quanta memoria un decoder può chiedere.** I reader zstd venivano costruiti
senza `WithDecoderMaxMemory`, il cui default di libreria è **64 GiB**: la
finestra la sceglie chi ha scritto il frame, e ogni frame che questo progetto
legge arriva da un'immagine che qualcun altro può pubblicare. Un frame valido
che dichiara una finestra enorme costa un centinaio di byte da produrre. Ora
tutti i reader — il codec in `pkg/compress` e i due lettori di metadati in
`pkg/index` — sono costruiti con `compress.MaxDecoderMemory` = **128 MiB**,
più di un ordine di grandezza sopra la finestra più larga che questo progetto
scrive (i livelli 1..4 arrivano a 8 MiB).

**Quanto plaintext un chunk può produrre.** La finestra non limita quanto un
flusso emette: un frame con finestra piccola può produrre byte all'infinito.
Il backup però dichiara quanto plaintext contiene ogni chunk — `pb` nel blob
privato sigillato, quindi autenticato — e la decompressione si ferma lì. Dove
quel valore non c'è ancora, il tetto è la dimensione massima del chunk
dichiarata dal manifest. Il limite non consegna il byte che dimostra il
superamento: chi legge di solito sta scrivendo altrove, e un rifiuto che
emette parte di ciò che rifiuta non è un rifiuto.

### Forma dell'indice dei file

`validateEntries` controllava path non vuoto, tipo noto, dimensioni non
negative, modo parsabile, digest esadecimale e link target presente. Non
controllava nulla di quel che i suoi lettori **assumono**:

| Vincolo | Perché |
| --- | --- |
| al massimo `MaxIndexEntries` (10 milioni) voci, verificato *mentre* si decodifica | il costo è la slice delle voci, non il blob compresso |
| path e link target al massimo `MaxPathBytes` (4096 byte) | è il più lungo che un filesystem Linux accetti: un nome più lungo non sarebbe comunque ripristinabile |
| un path compare una volta sola | due voci con lo stesso path rendono ambigua la selezione e priva di senso l'estensione della prima |
| gli offset tar crescono in senso stretto | il recupero parziale calcola la fine di una voce dall'inizio della successiva |

I due numeri assoluti non derivano da un campo del backup: quanti file
contiene è un dato riservato, che vive nei metadati sigillati e non è
disponibile mentre l'indice viene letto. Sono default motivati, e 10 milioni
di voci sono già un paio di gigabyte di oggetti vivi.

La copertura degli offset entro la fine del contenuto resta dov'era, in
`pkg/recovery/partial.go`: dipende dal totale in chiaro, che l'indice non
conosce.

### Il percorso remoto: conteggi annunciati e risorse per sessione

Le stesse regole valgono per il protocollo, dove la sorgente non fidata è il
peer invece del file.

**Layer annunciati (server).** `BackupStart.layer_count` è un `uint32` che
arrivava intatto fino a `make([]Layer, 0, start.LayerCount)`: un messaggio di
poche decine di byte chiedeva al processo la capacità per 4.294.967.295 layer,
circa 270 GB, prima che fosse spedito un solo byte di dati. Il tetto non è
inventato — è `maxDataLayers`, il budget overlayfs di un'immagine eseguibile,
che la pipeline di streaming applicava già mentre assembla i layer e che il
pianificatore del client non supera mai. Cambia solo il punto in cui viene
verificato: prima dell'allocazione, non dopo.

**Dimensione dei frame.** Era già corretta e resta la sola forma accettabile:
`protocol.ReadFrame` confronta la lunghezza dichiarata con
`protocol.MaxFrameSize` (4 MiB) **prima** di far crescere il buffer, e riusa
l'allocazione precedente quando basta.

**Scope e goroutine di rinnovo (client).** `maxSessionScopes` = 4 limita
*quali* credenziali una sessione può far coniare (A4.1). Non limitava *quante
volte*: ripetere l'unico scope a cui ha diritto non costa nulla al peer e
costa al client una `Provider.Get` — un giro di rete verso l'endpoint di
autorizzazione del registry, sull'account dell'utente — più il riavvio della
goroutine di rinnovo di quello scope, per ogni richiesta. Ora
`maxSessionTokenRequests` = `maxSessionScopes * 8` conta **tutte** le
richieste, comprese quelle malformate che la guardia rifiuta: quel che si
limita è il lavoro che il peer può causare, e una richiesta rifiutata è
comunque arrivata. Il default è motivato dal ciclo di rinnovo: il client
rinnova da solo a tre quinti della vita del token, quindi un server corretto
chiede una volta per scope e mai più; otto richieste per scope lasciano
margine a un registry che ruota le credenziali sotto un backup lungo. Le
goroutine vive restano una per scope — la precedente viene cancellata, non
affiancata — e il tetto è scritto anche dove le goroutine partono, perché un
limite che vive solo in un altro file è un limite che un chiamante futuro
aggira.

**Limiti annunciati verificati anche dal client.** `HelloAck.max_bytes` è la
quota che il server pubblica. Il client la confrontava solo con la stima, e
la stima è tale: sul percorso di streaming l'archivio viene prodotto mentre
viene spedito. Ora la quota annunciata è verificata anche sui byte realmente
inviati — layer per layer sul protocollo v1, frame per frame su quello v2 —
così un backup si ferma al limite dichiarato invece di scoprirlo dal rifiuto
del server, cioè dopo che i byte hanno attraversato la rete.
