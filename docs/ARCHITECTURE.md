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

File age (armored) con JSON: `{"dek":"<base64 32B>","nonceKey":"<base64 32B>","schemaVersion":1}`.
Destinatari: `scrypt` (passphrase) e/o `age1…` X25519.
Se la cifratura è attiva, senza passphrase o chiave privata il backup è
**irrecuperabile**.

## Pianificazione dei layer (overlayfs 127)

Limite overlayfs: 127 layer → massimo **118 layer di dati** (1 binario + 1
metadati + margine). Se la stima della dimensione supera il target, si
**aumenta la dimensione del layer**, mai il numero. `ChunkBytes =
clamp(layerBytes/64, 1 MiB, 64 MiB)`.