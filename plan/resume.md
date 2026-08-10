# backimage — Tracciamento avanzamento

> Aggiornare **dopo ogni sotto-fase verde**: spuntare la casella e aggiungere una riga al Log.
> Una sotto-fase è verde solo se `make check` esce 0 **e** tutte le voci della sua Definition of Done sono spuntate.

Stato progetto: **IN CORSO**
Versione corrente: `0.0.0`
Ultima fase completata: 04

---

## Fase 00 — Fondamenta, build a due stadi, harness CI
- [x] 00.1 Scheletro repo, `go.mod`, albero directory
- [x] 00.2 Makefile e script di verifica
- [x] 00.3 `.golangci.yml` e configurazione lint
- [x] 00.4 CI GitHub Actions (matrice, job root, job docker, job qemu)
- [x] 00.5 `internal/buildinfo` e `cmd/backimage` root cobra + `version`
- [x] 00.6 `internal/cli`: flag globali, output umano/JSON, codici di uscita, logging
- [x] 00.7 Build a due stadi con `go:embed` (placeholder)
- [x] 00.8 Scheletro `docs/` + `docs/DEPENDENCIES.md`
- [x] **Gate fase 00** (G1–G6, G9, G10, G11)

## Fase 01 — pkg/archive: fedeltà dei metadati
- [x] 01.1 Modello `Entry` e interfacce, `doc.go`
- [x] 01.2 Generatore di fixture ostili (`test/fixtures`)
- [x] 01.3 Lettura metadati Unix (stat, xattr, ACL, capabilities)
- [x] 01.4 Writer tar PAX (hardlink, device, FIFO, symlink, ordinamento deterministico)
- [x] 01.5 Reader/Extractor con ripristino metadati e ordine corretto
- [ ] 01.6 Backend Windows (`go-winio/backuptar`) e specificità macOS
- [x] 01.7 Modalità `--strict`, contabilità errori, preflight privilegi (`pkg/archive/preflight.go`: `PreflightBackup`/`PreflightRestore`, CapEff da `/proc/self/status`, rimedi; `--preflight` CLI resta per fase CLI)
- [ ] 01.8 Test di round-trip root-gated (`go test -tags root` richiede sudo; fixture root `FeatACLs|FeatCaps|FeatDevices|FeatFifos|FeatOwnership` implementati, e2e `phase_01.sh` pronto)
- [ ] **Gate fase 01** — G7 a 79.1% vs ≥85% (gap: rami EPERM root di `createOne`/`lchown`/`mknod`; completare con run `sudo -E go test -tags root` + `sudo make e2e PHASE=01`)

## Fase 02 — pkg/compress + pkg/chunk
- [x] 02.1 Interfaccia `Codec` e registro (5 codec; `UsageError` per livelli fuori range — mapping a exit code 2 via marker interface)
- [x] 02.2 Implementazioni gzip, zstd, xz, lz4 con livelli (xz: ulikunitz senza manopola livelli — accettato, documentato; lz4 non-powers: livelli 1..9 = `1<<(8+N)`)
- [x] 02.3 Interfaccia `Splitter` e splitter a dimensione fissa (buffer riusato, hash incrementale, AllocsPerRun=1)
- [x] 02.4 Planner dei layer con guardia 118 e auto-dimensionamento (tabella 7 casi + invariante 200 valori verdi)
- [x] 02.5 Benchmark (Core 7 240H) e `docs/compression.md` con numeri reali; sezione ARCHITECTURE "pianificazione dei layer" completa
- [x] **Gate fase 02** — G1–G6 (`make check` RC=0), G7 = 93.6% comb, GS-02.1..02.5 verdi (fuzz: CI 60 s; spot 15 s locale), G9 (3 moduli in DEPENDENCIES.md), G10 (`docs/compression.md`)
- [!] **Deviazione GS-02.5**: benchmark completo (256 MiB × xz) dura ~27 min → gate "< 10 min" violato per struttura, non per macchina. **Proposta**: GS-02.5 su `gzip|zstd|lz4|store` (< 2 min) + celle xz con corpus 32 MiB; xz documentato come sconsigliato (rischio noto in phase_02.md). Approvazione richiesta.

## Fase 03 — pkg/crypt
- [x] 03.1 Generazione DEK, struttura `KeyMaterial`, zeroizzazione (`key.go`: Wipe/zero, String/GoString REDACTED, Clone, Validate, JSON)
- [x] 03.2 Incapsulamento con age: passphrase scrypt + destinatari X25519 (`keyfile.go`: WrapKeys/UnwrapKeys, `ErrMixedRecipients`, due file `keys.age`+`keys.pass.age` dal CLI — deviazione nota, stesso KeyMaterial)
- [x] 03.3 Envelope del blob: header 12B/24B, nonce 12B, AAD con chunkIndex (magic `BIMGCHK1`, ver 1)
- [x] 03.4 Sealer/Opener per chunk (AES-256-GCM, tag 16B, overhead 40B, nil-km → envelope chiaro aead=0, `ErrIntegrity` → exit 5)
- [x] 03.5 Ingresso passphrase (`prompt.go`: Direct/File/Stdin/EnvVar/Prompt + Confirm, `openTTY` injettabile; `ErrNoPassphrase`/`ErrEmptyPassphrase`)
- [x] 03.6 Vettori di test: golden `keys.age` (testdata, passphrase testpass, DEK noto), envelope fuzz, golden vector convergente, bit-flip lane (100 giacoli), roundtrip suite
- [x] **Gate fase 03** — G1–G6 (`make check` RC=0 con golangci-lint via `$HOME/go/bin`), G7 = 90.0% su `pkg/crypt` (target ≥90%), fuzz ParseHeader 12 s + seeds, G9 (age/x/term in DEPENDENCIES.md già da fase 02), G10 (`docs/security.md`)

## Fase 04 — pkg/index + pkg/ociimg
- [x] 04.1 Modelli `Manifest`, `Chunks`, `Index` e serializzazione (index.go)
- [x] 04.2 Mappa offset→chunk e ricerca per path (`locator.go`)
- [x] 04.3 Costruttore di layer tar deterministici (contenuto rivisto in `layer.go`: mtime epoch, `LimitReader(size+1)` con errore "n payload bytes, want size", codec `store` → `OCIUncompressedLayer`)
- [x] 04.4 Assemblaggio immagine ggcr: `build.go` (`BuildImage`/`BuildIndex`), config, entrypoint, annotazioni, label (`mutate.Annotations` → type assert `(v1.Image)`), guard D02 (`errNonStandardCodec` su `MediaTypeSuffix()==""` + `Runnable`)
- [x] 04.5 Manifest list multi-arch con layer di dati condivisi (la verifica digest incrociata si fa in `BuildIndex`; il flag `--local-repo`/`--output` in confitto è stato rinviato al CLI fase 05)
- [x] 04.6 Output: `output.go` → `registry`, `daemon` (`pkg/v1/daemon`) , `oci-layout`, `tar` (select per host platform; `var daemonWrite` iniettabile per i test; vedi nota sotto)
- [x] 04.7 Test registry in-memory (httptest) + e2e `docker` (`test/e2e/phase_04.sh`, registry:2, blob unici 5/10 — 1 exe + 1 meta + 3 data, condivisi tra piattaforme)
- [x] **Gate fase 04** — G1–G6 (`make check` RC=0 con golangci-lint via `$HOME/go/bin`), G7 = 85.5% combinato (index 85.4% + ociimg ~85%), G10 (`docs/image-format.md` creato + `docs/ARCHITECTURE.md` aggiornato), G11 (resume aggiornato); GS-04.1..04.7 verdi; `make e2e PHASE=04` OK; `go mod tidy` (ggcr/otel/credentials). Deviazioni note: `daemon.Write` (ggcr v0.21.9) può andare in panic su certe immagini → nei test si inietta la variabile `daemonWrite` (mock), da rivalutare con upgrade ggcr.

## Fase 05 — pkg/registry + login + backup
- [x] 05.1 Keychain stratificata: store Docker-compatible 0600/lazy/atomico, Docker config + helper, alias Hub, errori non inghiottiti, bearer token pronto
- [x] 05.2 Comandi `login` / `logout`: verifica prima del salvataggio, stdin/TTY/token, conflitti, list JSON senza segreti
- [x] 05.3 Token provider effimero: margine 60 s, coalescenza successi/errori, auth anonima/basic/bearer, body 401 riavvolto e risposta chiusa
- [x] 05.4 Push parallelo: config+layer OCI, PATCH `Location`, backoff/429, manifest piattaforma prima dell'index, checkpoint serializzato e validato
- [x] 05.5 Comando `backup`: validazione usage, preflight registry anticipato, dry-run senza prompt/rete/scritture, layer file-backed, progresso/resume
- [x] 05.6 e2e `registry:2`: backup cifrato, `docker pull`, riuso blob, interruzione/ripresa, permessi, secret scan, refresh/coalescenza; slow test reale 2 GiB
- [x] **Gate fase 05** — G1–G6 (`make check` RC=0), G7 = 85.0% combinato (`pkg/registry` + `pkg/backup`), G8 (`make e2e PHASE=05` smoke verde; fixture default 2 GiB), GS-05.1..05.7 verdi, G10 (`docs/backup.md`, `docs/registries.md`), revisione Codex ultra completata

## Fase 06 — Binario auto-estraente
- [x] 06.1 Scheletro `cmd/backimage-selfextract` (stdlib `flag`), codici di uscita allineati e nessuna dipendenza vietata
- [x] 06.2 Sottocomando `info` (solo manifesto, umano/JSON)
- [x] 06.3 Sottocomando `list` (filtri, long, JSON, passphrase/age)
- [x] 06.4 Sottocomando `tar` (stdout binario pulito, guardia TTY, digest per chunk, memoria costante)
- [x] 06.5 Sottocomando `extract` (completo/selettivo, strip-components, parent e hardlink, <3 chunk nel test mirato)
- [x] 06.6 Sottocomando `verify` (parziale senza chiave, completo, continue, JSON)
- [x] 06.7 Budget dimensione + `go:embed` reale: amd64 4.219.042 B, arm64 3.997.858 B
- [x] 06.8 e2e `docker run` da host pulito, amd64 e arm64 con qemu temporaneo; tar/extract/list/info/verify e casi negativi verdi
- [x] **Gate fase 06** — `make check`, coverage comando 81,6%, e2e multiarch, ELF embedded, dimensione e dipendenze verdi; corretti anche digest wire `sha256:` e offset tar su padding differito

## Fase 07 — restore / inspect / verify / ls / doctor
- [x] 07.1 Source lazy da registry/OCI-layout/daemon, metadata-only e cache LRU 2 GiB verificata
- [x] 07.2 `restore` → tar (default), `--extract`, `--destination`, stdout e overwrite sicuri
- [x] 07.3 Restore parziale `--include` / `--exclude`, nuovo tar valido, parent/hardlink e meno di 3 chunk
- [x] 07.4 `inspect` (zero layer dati), `ls`, `find` con formato condiviso col selfextract
- [x] 07.5 `verify` quick/full/continue e mapping integrità
- [x] 07.6 `doctor` umano/JSON con rimedi ed exit 3 sui privilegi obbligatori
- [x] 07.7 Output `--json` valido su tutti i comandi
- [x] 07.8 E2E registry + OCI-layout: tar, extract completo/selettivo, parità list e stop pre-data su passphrase errata
- [x] **Gate fase 07 — MILESTONE v0.1.0** — `make check`, coverage combinata 85,7%, E2E e docs generati verdi
- [ ] Tag Git `v0.1.0` — differito alla consegna/release per non taggare un worktree non ancora committato

## Fase 08 — Trasporto TCP/TLS, protocollo, listen-remote
- [x] 08.1 Schema protobuf dei messaggi di controllo
- [x] 08.2 Framing e codec di frame
- [x] 08.3 Interfacce `Dialer`/`Listener`, impl. TCP+TLS1.3 e mTLS
- [x] 08.4 Macchina a stati della sessione lato server
- [x] 08.5 Flusso `TokenRefresh`
- [x] 08.6 Quote, ACL sui repo, limiti di concorrenza
- [x] 08.7 Client `--remote`: protocollo v2 streaming (default `--remote-mode stream`, pipeline interamente sul server; `layers` mantiene la v1)
- [x] 08.8 Ripresa da checkpoint dopo caduta di connessione
- [x] 08.9 e2e a due processi + fault injection
- [ ] **Gate fase 08**

## Fase 09 — Trasporto QUIC
- [x] 09.1 Impl. QUIC dietro la stessa interfaccia
- [x] 09.2 Flag `--udp` su client e server, ALPN, certificati
- [ ] 09.3 Tuning: stream, buffer, GSO (flag presenti; protocol v1 resta mono-stream)
- [x] 09.4 Harness di benchmark con `netem` (matrice RTT × loss)
- [ ] 09.5 `docs/transport-benchmark.md` con numeri e raccomandazione
- [ ] **Gate fase 09**

## Fase 10 — Dedup content-defined
- [x] 10.1 Splitter FastCDC/Rabin dietro `Splitter`
- [x] 10.2 Confini di layer content-defined
- [x] 10.3 Modalità nonce convergente e DEK stabile per repo
- [x] 10.4 Skip via `HEAD /blobs/<digest>` e statistiche
- [x] 10.5 e2e: backup, mutazione dell'1 %, secondo backup, asserzione sui byte caricati
- [ ] **Gate fase 10** (revisione crittografica Opus obbligatoria)

## Fase 11 — repo / retention / adapter vendor
- [ ] 11.1 Interfaccia `RegistryAdapter` con capability flag
- [ ] 11.2 Adapter OCI generico (DELETE per digest)
- [ ] 11.3 Adapter Docker Hub, GHCR, ECR, Quay
- [ ] 11.4 `repo ls` / `repo tags` / `repo rm` / `repo prune`
- [ ] 11.5 Motore di retention (keep-last/daily/weekly/monthly/yearly, `--dry-run`)
- [ ] 11.6 e2e su `registry:2` + API vendor mockate
- [ ] **Gate fase 11**

## Fase 12 — Documentazione, README, release
- [ ] 12.1 README utente finale con tutti i comandi
- [ ] 12.2 `docs/` completo (architettura, formati, sicurezza, troubleshooting)
- [ ] 12.3 Pagine man e completion shell
- [ ] 12.4 GoReleaser, firma, SBOM, checksum
- [ ] 12.5 Test di accettazione finale end-to-end
- [ ] **Gate fase 12 — MILESTONE v1.0.0**

---

## Log

| Data | Sotto-fase | Commit | Gate superati | Note |
|------|-----------|--------|---------------|------|
| 2026-08-08 | 00.1–00.8 | 9431748, 73b2d06, 2242169, 84be04f | G1–G6, G9, G10, G11 | fase 00 verde |
| 2026-08-08 | 01.1–01.7 | (worktree, da committare) | G10 (FIDELITY per-piattaforma + ordine ripristino) | non-root: make check + e2e verdi, coverage 79.1%; manca G7≥85% (root) e 01.6 Windows/macOS |
| 2026-08-09 | 02.1–02.5 | (worktree, da committare) | G1–G7, GS-02.1–GS-02.5, G9, G10 | fase 02 verde (93.6%); deviazione GS-02.5 da approvare (xz ~27 min) |
| 2026-08-09 | 03.1–03.6 | (worktree, da committare) | G1–G6, G7 (90.0% pkg/crypt), G9, G10 | fase 03 verde; G7 90.0%; openDevTTY coperto con tty reale; due keyfile age (scrypt+X25519 separati) |
| 2026-08-09 | 04.1–04.7 | (worktree, da committare) | G1–G6, G7 (85.5% comb), G10, G11 | fase 04 verde; e2e docker OK; doc image-format + ARCHITECTURE; rename `IndexRef`→`Ref`; nota daemon.Write v0.21.9 (panic → mock) |
| 2026-08-09 | 05.1–05.6 | (worktree, da committare) | G1–G8, GS-05.1–GS-05.7, G10, review | fase 05 verde; real registry pull; coverage 85.0%; slow 2 GiB peak heap 60 MiB; corretti config blob, ordine manifest, PATCH Location, retry body, checkpoint concorrenti e dry-run |
| 2026-08-09 | 06.1–06.8 | (worktree, da committare) | G1–G10, GS-06.1–GS-06.10 | fase 06 verde; selfextract 4,22/4,00 MB, coverage 81,6%, e2e amd64+arm64/qemu; restore selettivo; digest wire e TarOffset corretti |
| 2026-08-09 | 07.1–07.8 | (worktree, da committare) | G1–G10, GS-07.1–GS-07.7 | fase 07 verde; source lazy registry/layout/daemon, cache LRU, coverage 85,7%, e2e registry+layout; tag v0.1.0 differito alla release |
| 2026-08-09 | 08.1–08.9 (parziale) | (worktree, da committare) | G1–G10, GS-08.1, GS-08.3–GS-08.6, GS-08.8–GS-08.9 | protocollo protobuf e framing 4 MiB, TLS/mTLS/pin, ACL/quote/token refresh, registry diskless, ripresa e2e reale; manca GS-08.2 no-full-layer-spool e compressione server-side reale |
| 2026-08-09 | 09.1–09.5 (parziale) | (worktree, da committare) | G1–G6, G7 (85,3% transport), G8, GS-09.1–GS-09.3 | QUIC TLS1.3/ALPN, UDP+also-TCP, e2e registry/restart/crossed transports; harness creato e smoke reale, campagna netem 4 GiB non eseguita senza root |
| 2026-08-10 | 08.7 (streaming v2) | (worktree, da committare) | G1–G7 (85,4% su transport+protocol+server), G8 (`phase_08.sh` + `phase_08_stream.sh`), G10 | protocollo v2: `StreamStart/StreamAck/StreamEnd/StreamProgress`, pipeline (chunk/compress/seal/layer/dedup/push) sul server, indice file ricostruito dal tar con parità di offset/digest verso `archive.Writer`; client senza spool (picco 4 KiB su backup da 4 GiB, RSS ~19 MiB senza cifratura, ~280 MiB con scrypt), spool server 2× layer e ripulito; `--server-side-compress` ora coerente; e2e TCP+QUIC con restore verificato |
| 2026-08-09 | 10.1–10.5 | (worktree, da committare) | G7 (92,6% chunk), G8, GS-10.1, GS-10.4–GS-10.5, GS-10.7 | CDC Rabin con polinomio fisso, layer content-addressed/content-defined, DEK convergente riusata solo con manifest compatibile, metriche HEAD e `repo stats`; e2e 4 GiB: 4.304.158.550 → 1.043.787.983 B (24,25%), verify+restore di t1/t2 OK; manca soltanto revisione Opus GS-10.3/G11 |

---

## Blocchi aperti

- Fase 08: streaming v2 implementato e verificato (vedi log 2026-08-10). Restano
  aperti per il gate 08: campagna reale da 50 GiB con 1 GiB libero sul client
  (misurata solo a 256 MiB e 4 GiB) e ripresa a metà stream, che il protocollo v2
  non offre per scelta documentata (il retry rilegge la sorgente).
- Fase 09: l'harness richiede root+`tc` per la matrice netem da 4 GiB. I layer
  non possono essere distribuiti su più stream senza cambiare l'interfaccia
  mono-stream esplicitamente fissata dal piano; `--x-quic-streams` rifiuta quindi
  valori diversi da `1`.
- Fase 10: la revisione crittografica indipendente richiesta dal piano (Opus,
  G11/GS-10.3) non può essere auto-certificata dal codice. Il gate resta aperto
  fino a quella revisione.
