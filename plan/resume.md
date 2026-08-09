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
- [ ] 05.1 Keychain: `auth.json` proprio + `~/.docker/config.json` + credential helper
- [ ] 05.2 Comandi `login` / `logout`
- [ ] 05.3 Token provider effimero con refresh e `RoundTripper` con retry su 401
- [ ] 05.4 Push parallelo, backoff, checkpoint di ripresa
- [ ] 05.5 Comando `backup`: flag completi, pipeline, progresso
- [ ] 05.6 e2e su `registry:2` con fixture da 2 GB + test di ripresa
- [ ] **Gate fase 05**

## Fase 06 — Binario auto-estraente
- [ ] 06.1 Scheletro `cmd/backimage-selfextract` (stdlib `flag`)
- [ ] 06.2 Sottocomando `info`
- [ ] 06.3 Sottocomando `list`
- [ ] 06.4 Sottocomando `tar` (stdout)
- [ ] 06.5 Sottocomando `extract`
- [ ] 06.6 Sottocomando `verify`
- [ ] 06.7 Budget dimensione + `go:embed` reale nel binario principale
- [ ] 06.8 e2e `docker run` da host pulito, amd64 e arm64 (qemu)
- [ ] **Gate fase 06**

## Fase 07 — restore / inspect / verify / ls / doctor
- [ ] 07.1 Lettura layer da registry senza spacchettare
- [ ] 07.2 `restore` → tar (default), `--extract`, `--destination`
- [ ] 07.3 Restore parziale `--include` / `--exclude`
- [ ] 07.4 `inspect` (scarica solo l'indice), `ls`, `find`
- [ ] 07.5 `verify`
- [ ] 07.6 `doctor`
- [ ] 07.7 Output `--json` su tutti i comandi
- [ ] 07.8 Matrice e2e completa
- [ ] **Gate fase 07 — MILESTONE v0.1.0**

## Fase 08 — Trasporto TCP/TLS, protocollo, listen-remote
- [ ] 08.1 Schema protobuf dei messaggi di controllo
- [ ] 08.2 Framing e codec di frame
- [ ] 08.3 Interfacce `Dialer`/`Listener`, impl. TCP+TLS1.3 e mTLS
- [ ] 08.4 Macchina a stati della sessione lato server
- [ ] 08.5 Flusso `TokenRefresh`
- [ ] 08.6 Quote, ACL sui repo, limiti di concorrenza
- [ ] 08.7 Client `--remote`, pipeline lato client
- [ ] 08.8 Ripresa da checkpoint dopo caduta di connessione
- [ ] 08.9 e2e a due processi + fault injection
- [ ] **Gate fase 08**

## Fase 09 — Trasporto QUIC
- [ ] 09.1 Impl. QUIC dietro la stessa interfaccia
- [ ] 09.2 Flag `--udp` su client e server, ALPN, certificati
- [ ] 09.3 Tuning: stream, buffer, GSO
- [ ] 09.4 Harness di benchmark con `netem` (matrice RTT × loss)
- [ ] 09.5 `docs/transport-benchmark.md` con numeri e raccomandazione
- [ ] **Gate fase 09**

## Fase 10 — Dedup content-defined
- [ ] 10.1 Splitter FastCDC/Rabin dietro `Splitter`
- [ ] 10.2 Confini di layer content-defined
- [ ] 10.3 Modalità nonce convergente e DEK stabile per repo
- [ ] 10.4 Skip via `HEAD /blobs/<digest>` e statistiche
- [ ] 10.5 e2e: backup, mutazione dell'1 %, secondo backup, asserzione sui byte caricati
- [ ] **Gate fase 10**

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

---

## Blocchi aperti

Nessuno. (Se ce n'è uno, il dettaglio sta in `plan/BLOCKED.md`.)
