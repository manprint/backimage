# Bug ledger — piano astra

Difetti trovati durante l'esecuzione del piano che **non** appartengono a
nessuna fase. Ognuno ha un ID stabile, non riusato.

| ID | Trovato in | Stato | Sintesi |
| --- | --- | --- | --- |
| B-A001 | A1 (e2e) | APERTO | `restore --local-repo` non riesce a leggere nessun backup dal daemon Docker |
| B-A002 | A2.3 | APERTO | l'indice sostituisce con U+FFFD i byte non-UTF-8 dei nomi (il tar li conserva) |

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
