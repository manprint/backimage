# Compressione: risultati misurati

Misurato con `go test -bench=. -benchtime=1x ./pkg/compress/` (corpora 256 MiB
riproducibili, seed fissi; vedi `bench_test.go`).
Macchina di riferimento: Intel Core 7 240H (16 thread), Linux amd64, Go 1.26.

Formato celle: **compressione / decompressione, in MB/s / rapporto**

## gzip — livelli 1 / 6 (default) / 9

| Corpus | l1 | l6 | l9 |
|---|---|---|---|
| testo | 4457 / 8379 / 253× | 1681 / 9913 / 292× | 507 / 9889 / 294× |
| binario | 597 / 683 / 2.0× | 442 / 702 / 2.0× | 98 / 707 / 2.0× |
| incomprimibile | 6934 / 12101 / 1.0× | 6196 / 12578 / 1.0× | 252 / 13983 / 1.0× |

## zstd — livelli 1 / 2 (default) / 4

`--compression-level 0` significa "default del codec". Il manifest registra
il livello effettivamente applicato, quindi un backup zstd creato senza un
livello esplicito viene mostrato come livello 2, non come livello 0. Il livello
6 appartiene al default di gzip; zstd in backimage usa il range 1..4.

| livello | l1 | l2 | l4 |
|---|---|---|---|
| testo | 3987 / 4392 / 4565× | 4327 / 2880 / 8919× | 540 / 1981 / 10312× |
| binario | 2178 / 3309 / 2.0× | 1357 / 2595 / 2.0× | 62 / 2008 / 2.0× |
| incomprimibile | 3307 / 3958 / 1.0× | 3090 / 2441 / 1.0× | 660 / 2533 / 1.0× |

## xz — livelli 0 / 6 (default) / 9 — ⚠️ collo di bottiglia

| Livello | l0 | l6 | l9 |
|---|---|---|---|
| testo | 141 / 1440 / 6307× | 147 / 1539 / 6307× | 144 / 1527 / 6307× |
| binario | 6 / 24 / 2.0× | 6 / 24 / 2.0× | 6 / 24 / 2.0× |
| incomprimibile | 1 / 1919 / 1.0× | 1 / 1917 / 1.0× | 1 / 1923 / 1.0× |

`ulikunitz/xz` non espone alcuna manopola del livello: l0/l6/l9 producono
risultati identici. Il solo passaggio di compressione di 256 MiB binari
richiede **≈ 47 s** (5.7 MB/s) e per dati incomprimibili **≈ 212 s**
(1.3 MB/s). Con corpus 256 MiB, l'intera matrice `-bench=.` dura **~27 min**
(il gate GS-02.5 prevedeva < 10 min).

**Decisione (rischio noto del piano, fase 02)**: **xz è sconsigliato** per
backup operativi. Rimane disponibile per archivi a lungo termine su banda
scarsa dove il tempo di compressione è accettabile (testo/banca dati:
rapporto 6300× vs 290× di gzip). Il default resta zstd.

## lz4 — livelli 0 / 1 (default) / 9

| Livello | l0 | l1 | l9 |
|---|---|---|---|
| testo | 3856 / 3136 / 253× | 3632 / 3203 / 253× | 3991 / 3247 / 253× |
| binario | 1043 / 6433 / 2.0× | 160 / 4144 / 2.0× | 11 / 6780 / 2.0× |
| incomprimibile | 8651 / 5367 / 1.0× | 8538 / 5202 / 1.0× | 8877 / 5383 / 1.0× |

## store (nessuna compressione) — velocità di copia

Memcpy puro: 560–630 GB/s in scrittura, 60–90 GB/s dalla cache. Usato per
dati già compressi (`--compression none`).

---

## Raccomandazione

- **Default: zstd l2.** Migliore equilibrio velocità/rapporto, media type OCI
  standard, supporto overflow per layer > 2 GB.
- Dati già compressi (video, immagini, colonne `file_compressed`): `none` o
  `lz4` (marcatore 1.0×).
- Lungo termine su banda scarsa: `xz` accettabile per testo, sconsigliato per
  binario (compress ~6 MB/s).
- **Runnable**: lz4 e xz NON hanno media type OCI standard → immagine
  risultante non `docker run`-abile. Con `--runnable` (default) usare
  gzip | zstd. Vedere D02 in `plan/overview.md`.
- gzip livelli 6 vs 9: il 9 quasi raddoppia in 1.5× spazio (testo), ma è
  3–17× più lento — default l6.

## Come riprodurre

```sh
go test -bench=. -benchtime=1x ./pkg/compress/
```

Nota: xz rende la suite completa ~8 min; per un ciclo rapido escluderlo con
`-bench '(Compress|Decompress)/(gzip|zstd|lz4|store)'`.

---

## Influenza su fase 04 (assemblaggio immagine)

- La stima `bytesRaw * fattore` (fase 05) usa: zstd 0.45, gzip 0.50, xz 0.35,
  lz4 0.65, store 1.0. I rapporti misurati sulla riga `texto` sono assorti
  (0.1–0.4) su dati di log; su binario reale quelli sopra — il fattore è una
  stima conservativa.
- Il planner `pkg/chunk` non dipende dal codec (fase 04 ricomputa i layer se
  il flusso reale sfora il piano: vedi `docs/ARCHITECTURE.md`, sezione
  "pianificazione dei layer").
