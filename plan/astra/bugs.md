# Bug ledger — piano astra

Difetti trovati durante l'esecuzione del piano che **non** appartengono a
nessuna fase. Ognuno ha un ID stabile, non riusato.

| ID | Trovato in | Stato | Sintesi |
| --- | --- | --- | --- |
| B-A001 | A1 (e2e) | APERTO | `restore --local-repo` non riesce a leggere nessun backup dal daemon Docker |

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
