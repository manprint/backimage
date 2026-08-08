# Fase 12 — Documentazione, README, release — **milestone v1.0.0**

**Obiettivo**: consolidare la documentazione prodotta fase per fase, scrivere il README sintetico per l'utente finale con **tutti** i comandi, e mettere in piedi una release riproducibile e firmata.

---

## 12.1 README utente finale

**Agente: Haiku** (stesura), **Opus** (revisione)

### File: `README.md`

Struttura vincolante, in quest'ordine, con questi limiti di lunghezza:

| Sezione | Contenuto | Lunghezza massima |
|---|---|---|
| Intestazione | nome, una frase, badge CI e versione | 5 righe |
| Cos'è | 4–6 righe: backup dentro un'immagine Docker, auto-estraente, cifrata, push su qualunque registry | 6 righe |
| Perché | 4 punti: nessun server di backup da gestire, ripristino senza installare nulla, il registry fa da storage e da replica, cifratura di default | 8 righe |
| Installazione | binari precompilati, `go install`, build da sorgente | 15 righe |
| Avvio rapido | i **tre** comandi che coprono il 90 % dell'uso: backup, restore, `docker run … tar` | 20 righe |
| Comandi | **tabella completa** di ogni comando con una riga di descrizione | tabella |
| Il container auto-estraente | i 5 comandi con esempi copiabili | 25 righe |
| Cifratura | come funziona, come si recupera, l'avvertenza sulla perdita della passphrase in **grassetto** | 15 righe |
| Modalità remota | l'esempio a due comandi, con il rimando a `docs/remote.md` | 15 righe |
| Cosa viene preservato | tabella sintetica per piattaforma, con rimando a `docs/FIDELITY.md` | tabella |
| Limiti noti | elenco onesto (vedi sotto) | 10 righe |
| Documentazione | elenco dei file in `docs/` con una riga ciascuno | tabella |
| Licenza | | 2 righe |

### Tabella dei comandi (contenuto minimo)

```
backimage backup <PATH...> --repo <IMAGE>   crea un backup e lo pubblica
backimage restore <IMAGE>                   ripristina (per default deposita un .tar)
backimage inspect <IMAGE>                   metadati, layer e dimensioni
backimage ls <IMAGE> [PATH]                 elenca i file dentro il backup
backimage find <IMAGE> <PATTERN>            cerca file nel backup
backimage verify <IMAGE>                    verifica l'integrità
backimage doctor [PATH...]                  controlla privilegi e ambiente
backimage login/logout [REGISTRY]           credenziali del registry
backimage listen-remote --bind-address …    modalità server
backimage repo tags/rm/prune/stats/caps …   ciclo di vita dei backup
backimage version                           versione
backimage completion <shell>                completamento della shell
```

### Sezione "limiti noti" (deve essere sincera)

- `docker run` richiede al massimo 118 layer di dati: con backup molto grandi i layer diventano di parecchi GB (è il compromesso scelto per mantenere l'immagine eseguibile);
- l'estrazione dentro il container su Docker Desktop per macOS e Windows non preserva ownership e xattr: usare `tar` e poi estrarre sull'host;
- la dedup ha grana di layer, non di blocco: risparmia meno di restic o kopia a parità di modifiche;
- xz e lz4 producono immagini non eseguibili da Docker (`--runnable=false`);
- la cancellazione dei tag dipende dal registry: vedere `docs/registries.md`;
- perdere la passphrase significa perdere il backup: non esiste recupero.

### Definition of Done
- [ ] ogni comando elencato esiste davvero (`make docs-check` lo verifica)
- [ ] ogni blocco di esempio è stato eseguito e funziona
- [ ] il README sta sotto le 250 righe

---

## 12.2 Consolidamento di `docs/`

**Agente: Haiku** (stesura), **Sonnet** (verifica tecnica)

### Inventario finale atteso

| File | Origine | Verifica |
|---|---|---|
| `docs/ARCHITECTURE.md` | fasi 00, 02, 04 | corrisponde al codice |
| `docs/BUILD.md` | fase 00 | i comandi funzionano su un clone pulito |
| `docs/CONTRIBUTING.md` | fase 00 | contiene le dieci regole |
| `docs/DEPENDENCIES.md` | fase 00, aggiornato | coincide con `go.mod` |
| `docs/FIDELITY.md` | fase 01 | tabella per piattaforma, ordine di ripristino |
| `docs/compression.md` | fase 02 | numeri misurati |
| `docs/security.md` | fasi 03, 10 | modello di minaccia completo |
| `docs/image-format.md` | fase 04 | corrisponde al formato reale |
| `docs/backup.md` | fase 05 | tutti i flag |
| `docs/registries.md` | fasi 05, 11 | tabella capability × vendor |
| `docs/selfextract.md` | fase 06 | i comandi del container |
| `docs/restore.md` | fase 07 | le quattro strade a confronto |
| `docs/cli.md` | fase 07 | generato da cobra, rigenerabile senza diff |
| `docs/remote.md` | fasi 08, 09 | esempio completo con systemd |
| `docs/protocol.md` | fase 08 | corrisponde al `.proto` |
| `docs/transport-benchmark.md` | fase 09 | numeri misurati |
| `docs/dedup.md` | fase 10 | numeri dell'e2e |
| `docs/retention.md` | fase 11 | esempi di policy |
| `docs/troubleshooting.md` | **nuovo, questa fase** | vedi sotto |
| `docs/FAQ.md` | **nuovo, questa fase** | vedi sotto |

### `docs/troubleshooting.md` (nuovo)

Un'entrata per ogni codice di uscita e per ogni errore ricorrente, nel formato: sintomo → causa → rimedio. Coprire almeno:
- exit 3: privilegi insufficienti, in backup e in restore, su Linux/macOS/Windows;
- exit 4: passphrase errata o mancante, incluse le tre sorgenti alternative;
- exit 5: blob corrotto — cosa si può ancora recuperare (`--no-verify`, `verify --continue`, restore parziale);
- exit 6: registry irraggiungibile, token scaduto, rate limit, autenticazione;
- "no space left on device" durante il backup → spazio temporaneo, `--temp-dir`, `--max-layer-size`, modalità remota;
- `docker run` che fallisce con "max depth exceeded" → troppi layer, rifare con `--max-layer-size` più grande;
- estrazione su macOS che perde i permessi → usare `tar`;
- QUIC che non si connette → UDP bloccato, riprovare senza `--udp`.

### `docs/FAQ.md` (nuovo)

Almeno: perché un'immagine Docker e non un file tar; si può usare senza Docker (sì, il daemon non serve mai); quanto costa in spazio sul registry; è sicuro mettere un backup cifrato su un registry pubblico e cosa resta visibile; si può ripristinare su una macchina diversa da quella di origine; funziona con Kubernetes/CI; che differenza c'è con restic/kopia/velero.

### Definition of Done
- [ ] tutti i file dell'inventario esistono
- [ ] `make docs-check` esce 0
- [ ] nessun documento cita flag o comandi inesistenti

---

## 12.3 Pagine man e completamento della shell

**Agente: Haiku**

- `backimage completion bash|zsh|fish|powershell` (dal supporto nativo di cobra).
- Pagine man generate con `cobra/doc.GenManTree` nel target `make man`, incluse nella release come `backimage.1` e derivate.
- Le pagine generate **non** vanno committate (a differenza di `docs/cli.md`): si producono in fase di release.

### Definition of Done
- [ ] `backimage completion bash | bash -n` non dà errori di sintassi
- [ ] `make man` produce le pagine e `man ./dist/man/backimage.1` si apre

---

## 12.4 Release

**Agente: Sonnet**

### File: `.goreleaser.yaml`, `.github/workflows/release.yml`

### Prescrizioni
- La build di release passa da `make embed`: i binari self-extract veri devono essere incorporati. Un binario di release che restituisce `ErrNotEmbedded` è un difetto bloccante — aggiungere un controllo esplicito nel workflow.
- Piattaforme: `linux/{amd64,arm64,arm,riscv64}`, `darwin/{amd64,arm64}`, `windows/{amd64,arm64}`.
- `CGO_ENABLED=0`, `-trimpath`, ldflags con versione/commit/data.
- Archivi: `.tar.gz` per Unix, `.zip` per Windows; `checksums.txt` con SHA-256.
- SBOM in formato SPDX o CycloneDX per ogni archivio.
- Firma con `cosign` in modalità keyless (OIDC di GitHub Actions) di archivi e checksum.
- Immagine Docker di `backimage` stesso (non un backup: lo strumento), multi-arch, pubblicata su GHCR, base `gcr.io/distroless/static` o `scratch`.
- `CHANGELOG.md` aggiornato con la sezione `v1.0.0` che elenca le funzionalità per fase.

### Test di release (obbligatori nel workflow)
- scaricare l'archivio prodotto, estrarlo, eseguire `backimage version` e confrontare la versione con il tag;
- eseguire lo smoke test di 12.5 usando **il binario di release**, non quello compilato in CI.

---

## 12.5 Test di accettazione finale

**Agente: Sonnet**

### File: `test/e2e/acceptance.sh`

È lo scenario completo, dal punto di vista dell'utente, con il binario di release:

```
 1. doctor su una macchina pulita → exit 0
 2. login su un registry locale
 3. backup di un albero realistico (file di sistema copiati: /etc, con permessi e ACL)
    con cifratura, zstd, timestamp
 4. inspect → metadati corretti
 5. ls | wc -l → coincide con il conteggio dell'albero
 6. find di un file specifico → trovato
 7. verify → exit 0
 8. restore --extract in una directory nuova → ZERO differenze
 9. restore parziale di un solo file → ZERO differenze su quel file
10. docker pull da una seconda macchina simulata (cache Docker svuotata)
11. docker run IMAGE → info
12. docker run -i IMAGE tar > out.tar; estrazione sull'host → ZERO differenze
13. secondo backup con --dedup → byte caricati < 25 % del primo
14. repo tags → 2 tag; repo prune --keep-last 1 --yes → 1 tag
15. restore del tag superstite → ZERO differenze
16. backup verso un listen-remote in TLS con token → immagine identica
17. lo stesso con --udp → immagine identica
18. passphrase errata su ogni comando che la richiede → exit 4, sempre
19. tutti i comandi con --json | jq -e . → exit 0
20. tutti i comandi con --help → exit 0 e testo non vuoto
```

### Definition of Done
- [ ] `bash test/e2e/acceptance.sh` esce 0 usando un binario di release
- [ ] i punti 8, 9, 12, 15, 16, 17 danno zero differenze

---

## Gate di fase 12 — **MILESTONE v1.0.0**

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./...` | ≥ 80 % complessivo |
| G8 | `make e2e PHASE=all` | tutte le fasi 01–11 verdi |
| **GS-12.1** | `bash test/e2e/acceptance.sh` con binario di release | exit 0 |
| **GS-12.2** | `make docs-check` | exit 0, nessun comando o flag inesistente citato |
| **GS-12.3** | `docs/cli.md` rigenerato | nessuna differenza rispetto al committato |
| **GS-12.4** | binario di release: `SelfExtract("amd64")` | ELF valido, non placeholder |
| **GS-12.5** | `cosign verify-blob` sugli archivi | firma valida |
| **GS-12.6** | README | < 250 righe, tutti i comandi elencati esistono |
| **GS-12.7** | tutti i comandi con `--help` | exit 0, testo non vuoto |
| **GS-12.8** | `git grep -n "TODO\|FIXME\|XXX" -- '*.go'` | zero risultati **oppure** ognuno ha un issue aperto citato |
| G11 | revisione finale Opus + **tag `v1.0.0`** | approvazione in `resume.md` |

**Deliverable documentali**: `README.md` definitivo, `docs/troubleshooting.md`, `docs/FAQ.md`, `CHANGELOG.md` con la sezione `v1.0.0`, tutti i documenti dell'inventario 12.2 verificati.

**Rischi noti**
- Il rischio maggiore in questa fase è la documentazione che descrive un comportamento diverso da quello reale. `make docs-check` copre i nomi dei comandi, non la semantica: la revisione di Opus deve leggere `README.md` e `docs/selfextract.md` **eseguendo** ogni esempio.
- La firma keyless con cosign richiede permessi `id-token: write` nel workflow: se mancano, la release fallisce alla fine. Verificarlo su un tag di prova (`v0.9.0-rc1`) prima del rilascio vero.
