# Modelo di sicurezza

Versione: 1 · Aggiornato: fase 10 · Applicabile a: envelope `BIMGCHK1`, keyfile age, CLI (`--dedup`).

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
| DEK (256 bit) | `crypto/rand`, una per backup; riusata solo da `--dedup` con chiave precedente convergente | AES-256-GCM per ogni chunk |
| NonceKey (256 bit) | `crypto/rand`, insieme alla DEK | HMAC-SHA256 per derivazione nonce convergenti |
| Wrap (age scrypt) | passphrase utente | scrypt 2^18 (age); unwrap una tantum, mai per chunk |
| Wrap (age X25519) | coppia di chiavi utente | `keys.age` |

Il DEK NON viene mai derivato dalla passphrase (nessuna derivazione onerosa per
chunk); viene generato e avvolto. La perdita dell'identity/una delle due chiavi
comporta perdità irreversibile del backup. Questo è un trade-off deliberato:
l'avvolgimento age usa la stessa protezione dei chunk, con costo computazionale
spostato a unwrap singolo age singolo.

## Formato envelope per chunk (`BIMGCHK1`)

```
offset 0x00  magic 8B  "BIMGCHK1"
0x08        version 1B  = 1
0x09        codec   1B  (compress.ID: 0=store,1=gzip,2=zstd,3=xz,4=lz4)
0x0A        aead    1B   (0=none, 1=AES-256-GCM)
0x0B        flags   1B   (bit0 = convergent nonce)
0x0C..0x17  nonce   12B  (solo se aead=1)
0x18..       payload compresso + tag GCM (16B se aead=1)
```

Header totale: 24 byte (cifrato), 12 byte (chiaro, aead=0).
Overhead per chunk cifrato: 40 byte (24 header + 16 tag).

### Nonce (limiti GCM — CRITICO)

- Modalità default: 12 byte da `crypto/rand`, **mai riutilizzato** (verificato da
  test `TestNoncesNeverRepeat`). AES-GCM con chiave singola: limite pratico
  ≈ 2³² chunk cifrati con la stessa DEK; con 1 MiB/chunk → ≈ 4 EiB. Al di là
  ri-generare un nuovo backup (nuova DEK).
- Modalità convergente (`--dedup`, opt-in):
  `nonce = HMAC-SHA256(NonceKey, sha256(plaintext_chunk))[0:12]`. Per chunk
  identici con la stessa chiave → nonce e ciphertext identici → dedup. Il digest
  è quello del chunk in chiaro, prima di compressione e cifratura; la chiave HMAC
  impedisce di ricavare il nonce da un dizionario pubblico. Questa modalità
  rivela comunque l'uguaglianza dei chunk a chi osserva il registry.
- No riutilizzo nonce tra modalità: il client riusa una `KeyMaterial` solo se il
  manifest precedente dichiara già `nonceMode: convergent`; da `random` a
  `convergent` genera sempre una nuova chiave. GCM con nonce ripetuti sotto la
  stessa DEK sarebbe una perdita totale di confidenzialità.

### Trade-off della deduplica

`--dedup` non è attivo di default. Con esso, un osservatore del registry può
dedurre quali chunk e layer sono condivisi fra due backup e stimare quanto sono
cambiati i dati. Non ottiene il plaintext né la DEK. Usare la modalità normale
per dati con elevato rischio di analisi delle modifiche o con un avversario che
possa scegliere contenuti noti.

Il manifest espone solo `encryption.keyFingerprint`, i primi 8 byte di
`SHA-256(DEK)`: serve a verificare il riuso della chiave, non permette di
ricostruirla.

### AAD (authenticated data)

Per ogni chunk: `magic(8) | version | codec | aead | flags | uint32be(chunkIndex)`.
In modalità normale il chunkIndex è soggetto a AAD: un blocco spostato di
posizione viene rifiutato con `ErrIntegrity` (exit code 5). In modalità
convergente i quattro byte di indice sono zero: un confine CDC può spostare lo
stesso chunk a un altro indice nel backup successivo e legarlo all'indice
annullerebbe la dedup. Header, codec, flag e nonce restano autenticati; un chunk
con plaintext differente ha un nonce HMAC differente e non supera GCM.

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
