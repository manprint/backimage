# Bug ledger — piano astra

Difetti trovati durante l'esecuzione del piano che **non** appartengono a
nessuna fase. Ognuno ha un ID stabile, non riusato.

| ID | Trovato in | Stato | Sintesi |
| --- | --- | --- | --- |
| B-A001 | A1 (e2e) | APERTO | `restore --local-repo` non riesce a leggere nessun backup dal daemon Docker |
| B-A002 | A2.3 | APERTO | l'indice sostituisce con U+FFFD i byte non-UTF-8 dei nomi (il tar li conserva) |
| B-A003 | CI (job windows, aggiunto in A1) | RISOLTO | su Windows `readMeta` falliva se si chiedevano gli xattr: ogni entry veniva scartata e il backup usciva vuoto |
| B-A004 | CI (job macos, aggiunto in A1) | RISOLTO | fuori da Linux il writer non riconosceva hardlink e device e azzerava atime/ctime |

---

## B-A001 — `restore --local-repo` non legge alcun backup dal daemon Docker

**Trovato**: fase A1, mentre si cercava di includere il daemon fra le sorgenti
che devono rifiutare un'immagine falsificata. Il caso di controllo — un backup
**onesto** — fallisce già.

**Riproduzione**, con qualunque compressione:

```console
$ backimage backup ./t --repo bi-daemon-test --tag v1 --passphrase-file ./p \
    --output daemon --platform linux/amd64
$ backimage restore bi-daemon-test:v1 --local-repo -x -C ./out --passphrase-file ./p
error: tar read: chunk 0: data layer 0 tar: archive/tar: invalid tar header
```

Con `--compression zstd` (il default) l'errore è
`invalid input: magic number mismatch`, che è lo stesso guasto visto dal
decoder zstd.

**Causa accertata**: `daemon.Image` presenta **tutti** i layer come
`application/vnd.docker.image.rootfs.diff.tar.gzip` e ne ricomprime il
contenuto con gzip. Sonda su un'immagine caricata nel daemon:

```
layer 0 mt=application/vnd.docker.image.rootfs.diff.tar.gzip head=1f8b0800
layer 1 mt=application/vnd.docker.image.rootfs.diff.tar.gzip head=1f8b0800
layer 2 mt=application/vnd.docker.image.rootfs.diff.tar.gzip head=1f8b0800
```

I layer 0 e 1 di un'immagine backimage sono tar non compressi (codec `store`),
il layer dati è compresso col codec del backup: `docker save` conserva i blob
originali, ma il lettore tarball li tratta tutti come diff non compressi e li
gzippa. `pkg/restore/source.go:materialize` legge `Compressed()` e lo passa al
codec dichiarato in `archive.compression`, quindi ottiene un gzip che avvolge
un gzip (o un gzip dove si attendeva zstd).

**Perché non è stato corretto qui**: è anteriore al piano, non riguarda A01, e
la correzione è una scelta di progetto sul lettore daemon (quale accessorio di
layer usare, o se leggere l'indice OCI dello stream di `docker save` invece del
`manifest.json` di compatibilità) che merita la propria unità di lavoro.

**Impatto**: `--local-repo` in `restore`, `verify`, `ls`, `find`. Il percorso
non è coperto da nessuno script e2e, il che è il motivo per cui è rimasto
invisibile.

---

## B-A002 — l'indice non trasporta i nomi non-UTF-8

**Trovato**: fase A2.3, verificando che il produttore registri il nome byte per
byte «e che l'indice lo trasporti identico».

Il tar conserva i byte reali: il file viene archiviato e ripristinato intatto.
L'indice no. `index.json.zst` è JSON, e JSON non ha alcuna codifica per i byte
che non formano UTF-8 valido: `encoding/json` sostituisce ognuno con U+FFFD.

```
in  = "non-utf8-\xff\xfe.txt"
out = "non-utf8-��.txt"
```

**Impatto**: `ls`, `find` e il restore selettivo (`--include`/`--exclude`,
`StreamSelectedTar`) confrontano il nome dell'indice. Su un file con un nome
non-UTF-8 mostrano il nome sbagliato e non lo selezionano. Il restore completo
non è interessato.

**Perché non è stato corretto qui**: rappresentare quei byte richiede di
cambiare la codifica dei path nell'indice (campo affiancato in base64, o una
codifica tipo surrogateescape), cioè un **cambio di formato**. La 0.4.1
dichiara di non cambiare il formato immagine, e A6.0 congela le fixture dei
formati attuali prima di qualunque modifica: la correzione appartiene a quella
fase.

**Stato**: APERTO, limite documentato in `docs/FIDELITY.md` e fissato da
`TestNonUTF8NamesDoNotSurviveTheIndex` (`pkg/index`), che fallirà il giorno in
cui la codifica cambia.

---

## B-A003 — su Windows il backup non archiviava niente

**Trovato**: al primo giro dei job `windows` e `macos`, aggiunti alla CI in
fase A1. Prima di quei job nessun gate eseguiva la suite fuori da Linux, quindi
il difetto era invisibile.

**Sintomo**, sei test di `pkg/backup` più uno di `pkg/server`:

```
--- FAIL: TestPipelineToOCILayout
    result incoerente: {... Files:0 BytesRaw:0 BytesStored:16 Layers:1 Chunks:1 ...}
--- FAIL: TestFullReadBackCatchesCorruptedLayerOverHTTP
    the corrupted layer must be reported, got metadata tar: archive/tar: invalid tar header
--- FAIL: TestScanArchiveMatchesLocalWriter
    metadata "C:\...\001": xattrs are not supported on windows for "C:\...\001"
```

**Causa**: `pkg/archive/meta_windows.go` restituiva un errore quando
`Options.PreserveXattrs` era vero, e `PreserveXattrs` è vero per default in
`pkg/backup`. In `writer.emitOne` l'errore di `readMeta` passa per
`handleWalkError`, che in modo degradato registra e **salta la entry**: saltate
tutte, l'archivio conteneva solo la directory radice.

**Correzione**: Windows non ha attributi estesi POSIX, quindi chiederli non è
un errore — non c'è niente da perdere. `readMeta` compila i campi che la
piattaforma ha e non fallisce; il writer dichiara la lacuna una volta sola
(`xattrsSupported`, `warnOnce`) invece che a ogni entry.

**Correzione collegata**: un errore di `llistxattr` su una singola entry ora è
un `*xattrLossError`, che il writer conta come degradazione (`XattrsSkipped`)
lasciando la entry nell'archivio, invece di scartarla. Con `--strict` resta
fatale. È lo stesso difetto visto da un'altra angolazione: la fedeltà degli
attributi non deve poter cancellare un file.

---

## B-A004 — fuori da Linux il writer perdeva hardlink, device e tempi

**Trovato**: stesso giro di CI, job `macos`.

**Sintomo**:

```
--- FAIL: TestWriterHardlinkSinglePayload
    expected exactly 1 data payload in hard/, got 3
--- FAIL: TestReaderAtimeCtimeRoundTrip
    atime = 0001-01-01 00:00:00 +0000 UTC want 2024-03-09 16:00:01.444555666 +0000 UTC
    ctime not read
--- FAIL: TestRoundTrip/full-non-root
    [{Path:hard/orig.txt Field:hardlink-group Want:... Got:missing} ...]
```

**Causa**: `fileIdentity`, `unixFileDevice` e `statTimes` avevano una sola
implementazione reale, sotto `//go:build linux`. Il file di fallback copriva
`(unix && !linux) || windows` e restituiva sempre «non disponibile». Su darwin
i campi esistono, hanno solo nomi e ampiezze diverse (`Atimespec`/`Ctimespec`,
`Dev` con segno, `Nlink` a 16 bit).

**Effetto sui dati**: tre hardlink allo stesso inode venivano archiviati come
tre copie complete del contenuto — non una perdita di dati, ma una moltiplicazione
silenziosa della dimensione del backup e la perdita della struttura al restore.

**Correzione**: implementazioni reali in `file_identity_other_unix.go`,
`file_stat_other_unix.go` e `stat_times_other_unix.go`; il fallback che
restituisce «non disponibile» resta solo per Windows, dove Go non espone
alcuna identità di inode.

---

## Portabilità della suite (stessa unità di lavoro)

I due job nuovi hanno anche mostrato test che asserivano semantica Linux o di
POSIX su piattaforme che non ce l'hanno. Nessuno di questi era un difetto del
prodotto, e nessuna asserzione è stata rimossa su Linux:

| Test | Perché falliva fuori da Linux | Cosa è cambiato |
| --- | --- | --- |
| `TestExtract{Trusted,Security,UnsupportedNamespace}Xattr*` | `trusted.*` e `security.*` sono namespace del kernel Linux; altrove sono nomi qualunque | `t.Skip` fuori da Linux |
| `TestExtractHeterogeneousTreeNeverFails` | le due classi `xattr.*` non degradano su macOS | le classi attese includono `xattr.*` solo su Linux |
| `TestHostileNamesRoundTripByteForByte` | APFS rifiuta i nomi non UTF-8 (`EILSEQ`) | il nome rifiutato dal filesystem viene registrato e saltato |
| `TestWriterTarTvGated` | `tar` su macOS è bsdtar e rifiuta `--acls` in `-t` | il test richiede GNU tar |
| `TestReaderAtimeCtimeRoundTrip` | NTFS ha tick da 100 ns e nessun ctime | `t.Skip` su Windows |
| `TestExtractFifoAndSymlinkChain` | Windows non ha fifo | `t.Skip` su Windows |
| `TestOpenDevTTY` | `/dev/null` non esiste su Windows | usa `os.DevNull` |
| `TestGoldenFiles` | il checkout Windows riscriveva i golden in CRLF | `.gitattributes` fissa LF |
| `TestStoreAtomicWriteNoCorruption` | una directory Windows non ha bit di scrittura da togliere | `t.Skip` su Windows |
| `TestSpoolNamesAreUnique`, `TestSelfSignedCertificatePersistsInWorkDir` | Windows riporta sempre modo 666 | l'asserzione sul modo vale solo dove i permessi esistono |
| `TestNetworkErrorsAndAuthPath`, `TestAuthHomePromptAndDirectLogoutBranches` | separatore di path | `filepath.Join` nell'attesa |
| `TestPipelineTarRecipientAndTempCleanup` | l'output `tar` porta l'immagine della piattaforma host | il test costruisce per la piattaforma host |
| `TestBackup*ToOCILayout` | i job non costruivano gli asset self-extract | i due job li costruiscono prima dei test |
| `TestReceptionOverlapsTheRegistryPush` | misurava la ricezione dentro una finestra di 150 ms: la pipeline è profonda un layer, quindi un ricevente abbastanza veloce riempie il layer successivo *prima* che la finestra si apra e resta parcheggiato sul passaggio di consegne — pieno, non bloccato. Su Linux la finestra cadeva bene, su macOS e Windows no | il carico è di **due** layer, cioè quanto la pipeline profonda un layer può assorbire: la prima spinta viene trattenuta e il resto dello stream deve comunque arrivare al server. Serializzando le due metà il client si ferma al primo confine di layer |
| `TestFrameBufferOverlapsProductionWithSending` | confrontava il tempo trascorso con il costo seriale: su un runner lento il margine si chiude (303 ms contro un limite di 300 ms) | una spedizione viene trattenuta e si verifica che il produttore consegni comunque il frame successivo |
