# Modelo di sicurezza

Versione: 1 · Aggiornato: fase 03 · Applicabile a: envelope `BIMGCHK1`, keyfile age, CLI (`--passphrase`, `--convergent`).

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
| DEK (256 bit) | `crypto/rand`, una per backup | AES-256-GCM per ogni chunk (derivato da chiave estesa) |
| NonceKey (256 bit) | `crypto/rand`, una per backup | HMAC-SHA256 per derivazione nonce convergenti |
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
- Modalità convergente (`--convergent`, opt-in, CLI richiede conferma):
  nonce = HMAC-SHA256(KeyNonce, sha256(plaintext_chunk))[0:12]. Per chunk
  identici → nonce identico → ciphertext identico → dedup. Trade-off noto e
  accettato: rivela l'uguaglianza di blocchi ("chunk equality oracle"). Il
  nonce è derivato da KeyNonce + digest del payload **già compresso e cifrato**;
  non è possibile risalire al plaintext dal blocco. È vietato usare `--convergent`
  per dati di piccola cardinalità se l'avversario può scegliere plaintext noti.
- No riutilizzo nonce tra modalità: GCM con nonce ripetuti = perdita totale di
  confidenzialità. Le due modalità hanno header flags distinti; l'opener rifiuta
  flag sconosciuti.

### AAD (authenticated data)

Per ogni chunk: `magic(8) | version | codec | aead | flags (4 | 1+1+1+1) | uint32be(chunkIndex)`.
Il chunkIndex è SOGGETTO a AAD: un blocco spostato di posizione (reorder,
remap indice) viene rifiutato con `ErrIntegrity` (exit code 5).

## Trattamento dei segreti nel runtime

- `KeyMaterial` zera (wipe) DEK e KeyNonce in `Wipe()` (chiamato da
  cleanup nel roundtrip); `String()`/`GoString()` son REDACTED — mai stampare
  chiavi/passphrase nei log.
- Il file delle chiavi viene zeroed dopo uso (`zero(data)`).
- Memory: i support x/term disabilita echo (term.ReadPassword su /dev/tty).
- La passphrase NON viene mai loggata; il bootstrap del CLI non lo registra
  nemmeno con -v.

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