# Modelo di sicurezza

Versione: 2 · Aggiornato: 0.2.4 · Applicabile a: envelope `BIMGCHK1` v2, keyfile age, CLI (`--dedup`, `genpass`).

## Catena di elaborazione (ordine invariabile)

```
input → tar (pkg/archive) → compressione (pkg/compress) → cifratura (pkg/crypt) → chunking storage (pkg/chunk)
```

La cifratura avviene SEMPRE dopo la compressione: la deduplicazione a livello di
backup (layer planner) lavora su dati compressi, e la compressione preventiva
riduce la superficie di attacco laterale via side-channel di lunghezza.

## Flusso di chiavi

| Componente | Generazione | Uso |
|---|---|---|
| DEK (256 bit) | `crypto/rand`, una per backup; riusata solo da `--dedup` con chiave precedente convergente **ed envelope v2** | AES-256-GCM per ogni chunk |
| NonceKey (256 bit) | `crypto/rand`, insieme alla DEK | HMAC-SHA256 per derivazione nonce convergenti |
| Wrap (age scrypt) | passphrase utente (`backimage genpass`) | scrypt 2^18 (age); unwrap una tantum, mai per chunk |
| Wrap (age X25519) | coppia di chiavi utente | `keys.age` |

Il DEK NON viene mai derivato dalla passphrase (nessuna derivazione onerosa per
chunk); viene generato e avvolto. La perdita dell'identity/una delle due chiavi
comporta perdità irreversibile del backup. Questo è un trade-off deliberato:
l'avvolgimento age usa la stessa protezione dei chunk, con costo computazionale
spostato a unwrap singolo age singolo.

## Formato envelope per chunk (`BIMGCHK1`)

```
offset 0x00  magic 8B  "BIMGCHK1"
0x08        version 1B  (1 = legacy fino a 0.2.3, 2 = corrente)
0x09        codec   1B  (compress.ID: 0=store,1=gzip,2=zstd,3=xz,4=lz4)
0x0A        aead    1B   (0=none, 1=AES-256-GCM)
0x0B        flags   1B   (bit0 = convergent nonce)
0x0C..0x17  nonce   12B  (solo se aead=1)
0x18..       payload compresso + tag GCM (16B se aead=1)
```

Header totale: 24 byte (cifrato), 12 byte (chiaro, aead=0).
Overhead per chunk cifrato: 40 byte (24 header + 16 tag).

Il layout dei byte è identico nelle due versioni: cambiano solo la derivazione
del nonce convergente e la forma dei dati autenticati. La versione 1 continua a
essere **letta** (i backup già pubblicati si ripristinano intatti) e non viene
mai più **scritta**. Un `backimage` precedente alla 0.2.4 rifiuta un blob v2 con
`unsupported blob version 2 (support 1-2)`.

### Nonce (limiti GCM — CRITICO)

- Modalità default: 12 byte da `crypto/rand`, **mai riutilizzato** (verificato da
  test `TestNoncesNeverRepeat`). AES-GCM con chiave singola: limite pratico
  ≈ 2³² chunk cifrati con la stessa DEK; con 1 MiB/chunk → ≈ 4 EiB. Al di là
  ri-generare un nuovo backup (nuova DEK).
- Modalità convergente (`--dedup`, opt-in), envelope v2:
  `nonce = HMAC-SHA256(NonceKey, "backimage/nonce/v2\0" ‖ role ‖ sha256(payload_sigillato))[0:12]`.
  Il digest è quello dei byte che GCM cifra davvero, cioè il chunk **già
  compresso**. Per chunk identici con la stessa chiave → nonce e ciphertext
  identici → dedup. La chiave HMAC impedisce di ricavare il nonce da un
  dizionario pubblico. Questa modalità rivela comunque l'uguaglianza dei chunk a
  chi osserva il registry.
- No riutilizzo nonce tra modalità: il client riusa una `KeyMaterial` solo se il
  manifest precedente dichiara già `nonceMode: convergent` **e**
  `envelopeVersion: 2`; da `random` a `convergent`, o da una chiave legacy,
  genera sempre una nuova chiave. GCM con nonce ripetuti sotto la stessa DEK
  sarebbe una perdita totale di confidenzialità.

I blob di metadati (`index.json.zst`, `private.json.zst`) sono sigillati con lo
stesso schema e con un `role` distinto, quindi il nonce dipende dal contenuto e
dal tipo di blob.

#### Il riuso di nonce corretto nella 0.2.4 (era critico)

Fino alla 0.2.3 il nonce convergente era
`HMAC-SHA256(NonceKey, sha256(chunk_in_chiaro))`, mentre GCM cifrava il chunk
**compresso**. Le due cose non coincidono, e da questo seguiva che due backup con
la stessa chiave di repository potevano sigillare **byte diversi sotto lo stesso
nonce** ogni volta che la forma compressa di un chunk invariato cambiava:

- `--compression` o `--compression-level` diversi tra due esecuzioni;
- un aggiornamento del compressore che cambi l'output a parità di livello: le
  librerie di compressione non garantiscono stabilità dei byte fra versioni.

Il numero di worker dello zstd **non** è tra le cause: è stato misurato che
`zstd.WithEncoderConcurrency` non altera i byte prodotti (48 combinazioni di
dimensione, livello e worker, dati comprimibili e non). La proprietà è ora
bloccata da `TestZstdOutputIndependentOfWorkerCount` in `pkg/compress`, perché
la deduplica ci si appoggia.

Due messaggi AES-GCM sotto la stessa coppia (chiave, nonce) consegnano a chi
possiede le due immagini lo XOR dei due plaintext **e** la chiave di
autenticazione GHASH `H`, cioè la capacità di forgiare blob autenticati arbitrari
con quel DEK: cadono insieme confidenzialità e integrità.

Correzione: il nonce è derivato dai byte effettivamente cifrati, e
`crypt.Sealer.Seal` non accetta più un digest dal chiamante, così l'errore non è
più esprimibile nell'API. Nonce uguale ora implica payload uguale — esattamente
il caso che serve alla dedup, quindi la funzionalità è intatta.

Conseguenza operativa: una chiave che ha sigillato con la derivazione legacy è
trattata come **bruciata** e `--dedup` ne genera una nuova, perché il suo GHASH
può già essere compromesso. Il primo backup cifrato con `--dedup` dopo
l'aggiornamento ricarica tutti i blob una volta, poi la deduplica riprende.

Test di regressione: `TestConvergentNonceIsSealedPayloadDerived` e
`TestConvergentNonceIsRoleSeparated` in `pkg/crypt`,
`TestDedupRefusesLegacyEnvelopeKeyReuse` in `pkg/backup`,
`TestConvergentMetadataNonceIsContentDerived` in `pkg/index`.

### Trade-off della deduplica

`--dedup` non è attivo di default. Con esso, un osservatore del registry può
dedurre quali chunk e layer sono condivisi fra due backup e stimare quanto sono
cambiati i dati. Non ottiene il plaintext né la DEK. Usare la modalità normale
per dati con elevato rischio di analisi delle modifiche o con un avversario che
possa scegliere contenuti noti.

## Cosa è visibile senza la passphrase

Un backup cifrato non descrive il proprio contenuto in nessun dato pubblico.
Restano fuori dalla cifratura solo le informazioni necessarie a scaricare e
verificare i blob senza chiave:

| Visibile | Cifrato (`private.json.zst`) |
|---|---|
| data di creazione, versione tool, codec e livello | percorsi sorgente (`sources`) |
| `aead`, `nonceMode`, `kdf` | hostname, OS, arch della macchina di origine |
| digest e dimensione dei layer, numero di chunk | numero di file/dir/link e byte totali |
| per chunk: path del blob, `ss` e `sb` (digest e byte del **blob cifrato**) | per chunk: `ps` e `pb` (digest e byte del **plaintext**) |
| | impronta e recipient della chiave (`keyFingerprint`, `recipients`) |
| | elenco file, permessi, owner, mtime, digest (`index.json.zst`) |

Il digest del plaintext per chunk era il punto peggiore: pubblicato in chiaro
avrebbe permesso, senza toccare la crittografia, di confermare offline se un
file noto fa parte del backup. Ora vive nel blob privato e la verifica
plaintext è possibile solo dopo lo sblocco; `verify --quick` e la verifica
parziale del self-extract continuano a funzionare sui digest dei blob cifrati.
Le label OCI (`dev.backimage.sources`, `.files`, `.bytes-raw`), leggibili dal
registry senza scaricare l'immagine, non vengono pubblicate per un backup
cifrato.

Restano inevitabilmente osservabili: l'esistenza del backup, il momento in cui
è stato fatto, la sua dimensione complessiva e la dimensione di ogni blob
cifrato (quindi un profilo grossolano di comprimibilità), oltre a quanto
`--dedup` rivela per costruzione.

### AAD (authenticated data)

Envelope v2, per ogni blob:
`magic(8) | version | codec | aead | flags | role | uint32be(chunkIndex)` (17 byte).

`role` vale 0 per un chunk dati, 1 per `index.json.zst`, 2 per
`private.json.zst`. Fino alla 0.2.3 il ruolo non esisteva e i tre blob erano
sigillati con un AAD identico all'indice 0: sotto la stessa chiave uno
autenticava al posto dell'altro, e lo scambio arrivava fino al parser JSON. Ora
uno scambio di ruolo fallisce con `ErrIntegrity`.

In modalità normale il chunkIndex è soggetto a AAD: un blocco spostato di
posizione viene rifiutato con `ErrIntegrity` (exit code 5). In modalità
convergente i quattro byte di indice sono zero: un confine CDC può spostare lo
stesso chunk a un altro indice nel backup successivo e legarlo all'indice
annullerebbe la dedup. Header, codec, flag, ruolo e nonce restano autenticati; un
chunk con payload differente ha un nonce HMAC differente e non supera GCM.

Lo AAD **non** lega il blob a un singolo backup, e non può farlo: legare un
identificatore di backup renderebbe diversi due chunk identici e annullerebbe la
dedup fra tag, che è l'intero scopo di `--dedup`. Lo splice di un chunk fra due
backup che condividono la chiave è quindi respinto un livello più in alto, dai
digest del plaintext nel blob privato sigillato — che per questo il restore
verifica **sempre** su un backup cifrato (vedi sotto).

La versione 1 dello AAD (`16` byte, senza ruolo) è riprodotta byte per byte e
bloccata da `TestLegacyAADIsFrozen`: qualsiasi deriva impedirebbe ai backup
precedenti alla 0.2.4 di aprirsi.

### Verifica del plaintext non disattivabile (backup cifrati)

Da quando i digest del plaintext vivono nel blob privato sigillato (0.2.3), il
confronto per chunk non è più un controllo anti-corruzione barattabile con la
velocità: è ciò che rifiuta un chunk spostato tra due backup che condividono la
chiave di repository, il caso che lo AAD convergente non può coprire per
costruzione. Su un backup cifrato il confronto viene quindi eseguito sempre,
anche con `--no-verify`; su un backup in chiaro, dove ogni digest è pubblico,
`--no-verify` continua a valere come prima.
Regressione: `TestNoVerifyStillCatchesForgedChunk` in `pkg/recovery` costruisce
un blob forgiato che supera GCM, il controllo di dimensione e
`verify --quick`, e verifica che venga comunque respinto.

## La passphrase è l'unica difesa: dimensionarla di conseguenza

Il file chiavi viaggia **dentro l'immagine** (`keys.pass.age` nel layer
`/backup`). Chi possiede l'immagine possiede quindi il ciphertext della chiave e
può provare passphrase **offline**, senza limiti di tentativi e senza che nessun
log lo registri. Non esiste rate limit che possa aiutare: l'unica variabile è
quanto costa un tentativo e quanti tentativi servono.

Costo di un tentativo: age scrypt con `N=2^18, r=8, p=1`, cioè circa **256 MiB e
~1 s di CPU per tentativo**, con salt casuale di 16 byte. La memoria è ciò che
conta più del tempo: 256 MiB per candidato limitano severamente il parallelismo
su GPU e ASIC.

Tentativi necessari, se i caratteri sono scelti **a caso**:

| Passphrase | Entropia | Verdetto |
|---|---|---|
| 32 caratteri casuali, 4 classi (`genpass` default) | ~184 bit | fuori portata per sempre |
| 24 caratteri casuali, minuscole+cifre | ~124 bit | fuori portata |
| 24 caratteri di una frase in italiano | ~25-40 bit | **rotta in ore o giorni**, scrypt o no |

La riga da leggere è la terza: **lunghezza non è entropia**. Una frase inventata
e memorizzabile porta nell'ordine di 1-2 bit per carattere, quindi 24 caratteri
di prosa valgono una trentina di bit, non i 150 che l'aritmetica sui caratteri
suggerisce. Per questo:

- usare `backimage genpass` (32 caratteri, `crypto/rand`, senza bias di modulo) e
  conservare l'output in un password manager;
- `backimage backup` stima il lavoro di indovinamento e avvisa sotto i 96 bit
  (`crypt.MinRecommendedBits`). È un avviso, non un blocco: la scelta resta
  dell'utente e nessuno script esistente si rompe. Il messaggio non contiene mai
  la passphrase.

La passphrase è trattata **byte per byte**, senza normalizzazione Unicode e senza
trim oltre al singolo newline finale di `--passphrase-file` e
`--passphrase-stdin`. Punteggiatura ASCII completa, spazi interni e finali, `\r`
incorporato e UTF-8 multibyte arrivano intatti a scrypt su tutte le sorgenti
(`--password`, `--passphrase-file`, `--passphrase-stdin`,
`BACKIMAGE_PASSPHRASE`, prompt su `/dev/tty`); il contratto è bloccato da
`TestReadPassphrasePreservesEveryByte` e
`TestWrapUnwrapWithSpecialCharacters`. Due conseguenze pratiche: una passphrase
non può contenere un newline se passa da file o stdin, e una passphrase con
caratteri combinanti Unicode deve essere consegnata sempre nella stessa forma di
normalizzazione (digitarla a mano dopo averla generata NFC/NFD può non
coincidere) — motivo in più per usare `genpass` e un password manager.

## Limite noto: il binario `/backimage` non è firmato

Questo non si può chiudere dentro il formato, e va detto chiaramente.

Il layer 0 dell'immagine contiene il binario self-extract che `docker run`
esegue. Non è firmato e non esiste una firma sull'immagine. Chi ottiene
l'immagine può sostituire `/backimage` con una versione che esfiltra la
passphrase e restituirla alla vittima (registry compromesso, tag mutabile,
backup condiviso). La crittografia non viene attaccata: è l'utente a consegnare
la passphrase al binario dell'attaccante.

Nessun dato dentro l'immagine può impedirlo — un binario manomesso semplicemente
non esegue il controllo che gli si chiede di fare. La difesa è **fuori banda**:

- ripristinare con un `backimage` locale e fidato (`backimage restore IMAGE`),
  non con l'entrypoint dell'immagine, quando l'immagine ha attraversato un
  perimetro non controllato;
- riferire l'immagine **per digest** e non per tag (`repo@sha256:...`);
- firmare l'immagine con cosign e verificare la firma prima del ripristino;
- trattare un registry a cui altri possono scrivere come non fidato.

`docker run IMAGE restore` resta il percorso comodo e va bene per un'immagine
che non ha mai lasciato un perimetro fidato.

## Trattamento dei segreti nel runtime

- `KeyMaterial` zera (wipe) DEK e KeyNonce in `Wipe()` (chiamato da
  cleanup nel roundtrip); `String()`/`GoString()` son REDACTED — mai stampare
  chiavi/passphrase nei log.
- Il file delle chiavi viene zeroed dopo uso (`zero(data)`).
- Memory: i support x/term disabilita echo (term.ReadPassword su /dev/tty).
- La passphrase NON viene mai loggata; il bootstrap del CLI non lo registra
  nemmeno con -v.

## Backup remoto: dove risiedono le chiavi

`--remote` ha due modalità con confini di fiducia diversi (dettagli in
[remote.md](remote.md)).

| | `--remote-mode stream` (default) | `--remote-mode layers` |
| --- | --- | --- |
| Passphrase | resta sul client | resta sul client |
| `keys.age` / `keys.pass.age` | prodotti dal client, inviati già wrappati | prodotti dal client |
| DEK e NonceKey | **inviati al server** nel messaggio `StreamStart` | mai inviati |
| Dati in chiaro | **visibili al server** (è lui che cifra) | mai visibili |
| Credenziali registry | token bearer effimeri e scoped, in memoria del server | idem |

Conseguenze operative della modalità `stream`:

- il server remoto è dentro il perimetro di fiducia del contenuto del backup:
  va trattato come un host che vede i dati, non come un semplice relay;
- DEK e NonceKey vivono solo nella memoria della sessione (`KeyMaterial.Wipe()`
  alla chiusura) e non finiscono mai in `--work-dir` né nei log;
- lo spool di layer del server contiene chunk già compressi e cifrati, con
  permessi 0600, ed è rimosso anche sui percorsi di errore e cancellazione;
- chi non può concedere questa fiducia deve usare `--remote-mode layers`, che
  mantiene l'intera pipeline crittografica sul client.

## Verifica d'integrità

- GCM: autenticazione AES-GCM (tag 16B) per ogni blocco — la cifratura
  autenticata rileva qualsiasi modifica al ciphertext, al nonce o all'header.
- Testing: 100 bit flip casuali in ogni blocco → sempre `ErrIntegrity` o
  header error; nessun garantito plaintext leak.
- Blob cifrato aperto senza DEK → errore "encrypted blob: key material required"
  (mai rivelare se il passphrase è sbagliato: messaggio unico per
  passphrase-vs-file corrotto, niente oracle di verifica).

## Exit codes CLI (contratto)

| Codice | Significato |
|---|---|
| 0 | successo |
| 4 | passphrase mancante/sbagliata (`ErrPassphrase`/`ErrWrongPassphrase`) |
| 5 | integrità fallita (`ErrIntegrity`) — dati manomessi |

## Recoverability

- Senza DEK (perdita di entrambi identity `keys.age` AND `keys.pass.age` +
  passphrase): backup inrecuperabile — nessun backdoor di design.
- `keys.age` e `keys.pass.age`: scrivere su supporti separati, proteggere
  l'identity age con chiave hardware se disponibile.
- Ruolo di `NonceKey`: serve solo alla derivazione dei nonce convergenti; la perdita
  della NonceKey non impedisce la decifratura dei chunk esistenti (random nonce e GCM non la usano); invalida solo
  i nonce convergenti futuri (revoca della dedup per nuovi chunk).

## Note implementative

- age v1.3.1 non mixa scrypt + X25519 nello stesso file (crittografo
  li tratta separatamente): il CLI produce DUE key file `keys.age`
  (X25519) + `keys.pass.age` (scrypt) con lo stesso KeyMaterial;
  `WrapKeys` rifiuta recipient misti.
- `pkg/crypt` dipende SOLO da stdlib + `filippo.io/age` + `golang.org/x/term`
  (no import on circolari): il self-extract può usarlo direttamente.
- Messaggi di errore: messaggio unico `wrong passphrase or key` anche per file
  corrotto (`age.Decrypt` mancante / JSON rotto → stesso sentinel): nessun
  oracolo per l'attaccante.
