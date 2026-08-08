# backimage — Tracciamento avanzamento

> Aggiornare **dopo ogni sotto-fase verde**: spuntare la casella e aggiungere una riga al Log.
> Una sotto-fase è verde solo se `make check` esce 0 **e** tutte le voci della sua Definition of Done sono spuntate.

Stato progetto: **NON INIZIATO**
Versione corrente: `0.0.0`
Ultima fase completata: —

---

## Fase 00 — Fondamenta, build a due stadi, harness CI
- [ ] 00.1 Scheletro repo, `go.mod`, albero directory
- [ ] 00.2 Makefile e script di verifica
- [ ] 00.3 `.golangci.yml` e configurazione lint
- [ ] 00.4 CI GitHub Actions (matrice, job root, job docker, job qemu)
- [ ] 00.5 `internal/buildinfo` e `cmd/backimage` root cobra + `version`
- [ ] 00.6 `internal/cli`: flag globali, output umano/JSON, codici di uscita, logging
- [ ] 00.7 Build a due stadi con `go:embed` (placeholder)
- [ ] 00.8 Scheletro `docs/` + `docs/DEPENDENCIES.md`
- [ ] **Gate fase 00** (G1–G6, G9, G10, G11)

## Fase 01 — pkg/archive: fedeltà dei metadati
- [ ] 01.1 Modello `Entry` e interfacce, `doc.go`
- [ ] 01.2 Generatore di fixture ostili (`test/fixtures`)
- [ ] 01.3 Lettura metadati Unix (stat, xattr, ACL, capabilities)
- [ ] 01.4 Writer tar PAX (hardlink, device, FIFO, symlink, ordinamento deterministico)
- [ ] 01.5 Reader/Extractor con ripristino metadati e ordine corretto
- [ ] 01.6 Backend Windows (`go-winio/backuptar`) e specificità macOS
- [ ] 01.7 Modalità `--strict`, contabilità errori, preflight privilegi
- [ ] 01.8 Test di round-trip, inclusi test gated `root`
- [ ] **Gate fase 01**

## Fase 02 — pkg/compress + pkg/chunk
- [ ] 02.1 Interfaccia `Codec` e registro
- [ ] 02.2 Implementazioni gzip, zstd, xz, lz4 con livelli
- [ ] 02.3 Interfaccia `Splitter` e splitter a dimensione fissa
- [ ] 02.4 Planner dei layer con guardia 127 e auto-dimensionamento
- [ ] 02.5 Benchmark e tabella comparativa in `docs/compression.md`
- [ ] **Gate fase 02**

## Fase 03 — pkg/crypt
- [ ] 03.1 Generazione DEK, struttura `KeyMaterial`, zeroizzazione
- [ ] 03.2 Incapsulamento con age: passphrase scrypt + destinatari X25519
- [ ] 03.3 Envelope del blob: header, nonce, AAD
- [ ] 03.4 Encryptor/Decryptor per chunk
- [ ] 03.5 Ingresso passphrase (tty, env, stdin, file) in `pkg/crypt/prompt`
- [ ] 03.6 Vettori di test, golden file, fuzz su envelope
- [ ] **Gate fase 03**

## Fase 04 — pkg/index + pkg/ociimg
- [ ] 04.1 Modelli `Manifest`, `Chunks`, `Index` e serializzazione
- [ ] 04.2 Mappa offset→chunk e ricerca per path
- [ ] 04.3 Costruttore di layer tar deterministici
- [ ] 04.4 Assemblaggio immagine ggcr: config, entrypoint, annotazioni, label
- [ ] 04.5 Manifest list multi-arch con layer di dati condivisi
- [ ] 04.6 Output: `registry`, `daemon`, `oci-layout`, `tar`
- [ ] 04.7 Test con registry in-memory + e2e `docker inspect`
- [ ] **Gate fase 04**

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
| | | | | |

---

## Blocchi aperti

Nessuno. (Se ce n'è uno, il dettaglio sta in `plan/BLOCKED.md`.)
