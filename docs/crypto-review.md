# Dossier per una review crittografica indipendente

Questo documento esiste per una sola ragione: rendere **affrontabile da un
revisore esterno** la modalità convergente di backimage, cioè la deduplica
`--dedup`. Non è una dichiarazione di sicurezza e non chiude il rischio
residuo: raccoglie in un solo posto le derivazioni, i dati autenticati, le
decisioni sulle chiavi e i vettori di prova, con il puntatore al codice e al
test che fissa ciascuna affermazione.

Versione dell'albero descritta: **0.5.0**, envelope **v3**, schema del
materiale di chiave **2**.

Documenti adiacenti, che questo non sostituisce:
[security.md](security.md) (modello di minaccia e superficie completa),
[image-format.md](image-format.md) (formato su disco),
[dedup.md](dedup.md) (uso e costi operativi).

---

## 1. Cosa si chiede al revisore

Nell'ordine in cui conviene leggerlo:

1. **La derivazione del nonce convergente della §5.3 è sana?** In particolare:
   la troncatura a 96 bit di un HMAC-SHA256, con l'AAD e il digest del payload
   sigillato come ingressi, e la chiave HMAC separata dalla chiave AES.
2. **Il perimetro di riuso della §7 è quello giusto?** Una `KeyMaterial`
   può sigillare più backup; l'unica autorità sul riuso è l'attestazione dentro
   l'involucro age. Le condizioni sono: stessa epoca dell'envelope, stessa
   modalità nonce, politica `convergent-dedup`.
3. **L'esclusione deliberata di `chunkIndex` dall'AAD convergente (§6) è
   compensata a sufficienza** dai digest del plaintext nel blob privato
   sigillato?
4. **Il bilancio delle collisioni della §9** regge per le dimensioni d'uso
   dichiarate?
5. **La deduplica rivela quel che la §8 dichiara, e nulla di più?**

Ciò che questo documento **non** chiede: una review dell'uso di AES-256-GCM
per sé, di `filippo.io/age`, di scrypt o di `crypto/rand`. Sono componenti
esterne usate nella loro modalità documentata.

---

## 2. Primitive e parametri

| Ruolo | Primitiva | Parametri |
| --- | --- | --- |
| Cifratura dei blob | AES-256-GCM (`crypto/cipher`, `NewGCM`) | chiave 32 B, nonce 12 B, tag 16 B |
| Derivazione del nonce convergente | HMAC-SHA256 troncato | chiave 32 B (`NonceKey`), output 12 B |
| Nonce in modalità normale | `crypto/rand` | 12 B per blob |
| Avvolgimento del materiale di chiave | age v1 (`filippo.io/age`) | destinatario scrypt (`N=2^18, r=8, p=1`) oppure X25519 |
| Digest di contenuto e metadati | SHA-256 | — |

Nessuna primitiva è implementata in questo repository: sono la libreria
standard Go e `filippo.io/age`.

---

## 3. Gerarchia delle chiavi

```
passphrase utente  ──age scrypt──┐
                                 ├──▶  keys.pass.age  ──▶  KeyMaterial ──┬── DEK      (32 B)  AES-256-GCM
identità age X25519 ─────────────┘      keys.age                         └── NonceKey (32 B)  HMAC-SHA256
```

- `KeyMaterial` è un oggetto JSON (`pkg/crypt/key.go`) avvolto da age. I due
  file contengono lo **stesso** materiale per due destinatari diversi: age v1
  non mescola scrypt e X25519 in un solo file.
- La passphrase non cifra mai un chunk. Viene usata una volta per aprire
  l'involucro; da lì in poi si lavora con DEK e NonceKey.
- DEK e NonceKey sono generate indipendentemente da `crypto/rand`
  (`newKeyMaterial`), mai derivate l'una dall'altra.
- `KeyMaterial.Wipe()` azzera entrambe; i chiamanti la invocano con `defer`.
- **Impronta della chiave**: `hex(sha256(DEK)[0:8])`, scritta nel blob privato
  sigillato (`pkg/backup/pipeline.go:keyFingerprint`). Serve a riconoscere
  quale materiale ha sigillato un backup; non viaggia mai in chiaro. È un
  digest diretto di un segreto da 256 bit: un attaccante che la ottenesse non
  ne ricava un vantaggio praticabile, ma è un punto su cui vale un parere.

Il JSON avvolto, schema 2:

```json
{
  "schemaVersion": 2,
  "envelopeVersion": 3,
  "nonceMode": "convergent",
  "reuse": "convergent-dedup",
  "dek": "<32 byte, base64>",
  "nonceKey": "<32 byte, base64>"
}
```

I tre campi di attestazione sono la novità della 0.5.0 e sono il soggetto
della §7. Lo schema 1 (fino alla 0.4.0) ha solo `dek` e `nonceKey`: si apre
ancora, non si riusa mai.

---

## 4. Envelope `BIMGCHK1`

Ogni blob memorizzato — chunk di dati, `index.json.zst`, `private.json.zst` —
ha questa forma (`pkg/crypt/envelope.go`):

```
offset  len  campo
 0       8   magic    "BIMGCHK1"
 8       1   version  1 | 2 | 3
 9       1   codec    identificatore del compressore
10       1   aead     0 = nessuno, 1 = AES-256-GCM
11       1   flags    bit0 = nonce convergente
12      12   nonce    presente solo se aead != 0
24       -   payload  (compresso, poi cifrato) ‖ tag GCM 16 B
```

Il layout dei byte è identico nelle tre versioni. Cambia soltanto il
significato dei dati autenticati e la derivazione del nonce.

Un backup **non cifrato** non ha envelope affatto: i blob sono il flusso
compresso puro (`pkg/backup/pipeline.go`, ramo senza sealer). La distinzione è
netta di proposito e vale in entrambe le direzioni: il lettore di un backup
cifrato rifiuta un blob privo di tag AEAD, quello di un backup dichiarato non
cifrato rifiuta un blob che sia un envelope. Quale dei due lettori si usa è
deciso una volta dal manifest, mai da quel che l'header di un blob dichiara di
essere — l'header lo scrive chi ha scritto il blob
(`NewKeyedOpener` / `NewClearOpener`, `pkg/crypt/chunk.go`).

| Versione | Scritta da | AAD | Nonce convergente |
| --- | --- | --- | --- |
| 1 | fino a 0.2.3 | 16 B, senza ruolo | `HMAC(NonceKey, sha256(plaintext))` |
| 2 | 0.2.4 – 0.4.0 | 17 B, con ruolo | `HMAC(NonceKey, "backimage/nonce/v2\0" ‖ role ‖ sha256(payload sigillato))` |
| 3 | 0.5.0 | 17 B, con ruolo | `HMAC(NonceKey, "backimage/nonce/v3\0" ‖ AAD ‖ sha256(payload sigillato))` |

Le versioni 1 e 2 sono ancora **lette** (fixture congelate in
`pkg/recovery/testdata`, vettori golden in `pkg/crypt/golden_test.go`) e non
sono più **scritte**.

### 4.1 Composizione dell'AAD

Versione 1 (16 byte), riprodotta byte per byte e bloccata da
`TestLegacyAADIsFrozen`:

```
magic(8) ‖ version(1) ‖ codec(1) ‖ aead(1) ‖ flags(1) ‖ uint32be(chunkIndex)
```

Versioni 2 e 3 (17 byte):

```
magic(8) ‖ version(1) ‖ codec(1) ‖ aead(1) ‖ flags(1) ‖ role(1) ‖ uint32be(chunkIndex)
```

In modalità **convergente** i quattro byte di `chunkIndex` sono zero (§6). Il
nonce **non** è nell'AAD: viaggia in chiaro nell'header e GCM già lo lega.

`role` vale 0 per un chunk di dati, 1 per l'indice dei file, 2 per i metadati
riservati. Fino alla 0.2.3 il ruolo non esisteva e i tre blob erano sigillati
con un AAD identico all'indice 0: sotto una stessa chiave uno autenticava al
posto dell'altro fino al parser JSON.

---

## 5. Derivazione del nonce

### 5.1 Modalità normale (default)

12 byte da `crypto/rand` per blob. `chunkIndex` è dentro l'AAD, quindi un
blocco spostato di posizione fallisce l'autenticazione.

### 5.2 Le due correzioni storiche

**Fino alla 0.2.3 (v1).** `nonce = HMAC-SHA256(NonceKey, sha256(chunk in
chiaro))`, mentre GCM cifrava il chunk **compresso**. Le due cose non
coincidono: due esecuzioni con la stessa chiave di repository sigillavano byte
diversi sotto lo stesso nonce ogni volta che la forma compressa di un chunk
invariato cambiava — `--compression` o `--compression-level` diversi, oppure
un aggiornamento del compressore che riscrive l'output a parità di livello.

**Fino alla 0.4.0 (v2).** Il nonce derivava da `role ‖ sha256(payload
sigillato)` con un'etichetta di dominio costante, mentre l'AAD copriva
l'header intero. Due blob con lo **stesso payload** e un header **diverso**
ricevevano quindi lo stesso nonce e AAD diversi: di nuovo due messaggi GCM
sotto la stessa coppia (chiave, nonce). L'innesco realistico non era `codec`
— un codec diverso produce byte diversi, quindi un nonce diverso — ma
`version`: l'etichetta era la costante `"backimage/nonce/v2\0"`, e il giorno
in cui la versione fosse cambiata senza toccarla, su un repository con chiave
riusata e `--dedup`, la collisione sarebbe stata sistematica. È esattamente
quello che questa release fa.

In entrambi i casi la conseguenza è la stessa e va detta per intero: due
messaggi AES-GCM sotto una coppia (chiave, nonce) consegnano a chi possiede
le due immagini lo XOR dei due plaintext **e** la chiave di autenticazione
GHASH `H`, cioè la capacità di forgiare blob autenticati arbitrari con quel
DEK. Cadono insieme confidenzialità e integrità.

### 5.3 Envelope v3, la derivazione attuale

```
nonceLabel = "backimage/nonce/v" ‖ decimal(envelopeVersion) ‖ 0x00
aad        = AAD(header, role, chunkIndex)            // §4.1
nonce      = HMAC-SHA256(NonceKey, nonceLabel ‖ aad ‖ sha256(payload))[0:12]
```

dove `payload` sono **esattamente i byte che AES-GCM sta per cifrare**, cioè
il chunk già compresso. Codice: `convergentNonce` in `pkg/crypt/chunk.go`;
l'AAD è calcolato prima del nonce in `sealer.Seal` e lo stesso valore è
passato a `Seal` di GCM.

Due proprietà volute:

- **`nonce` uguale ⇒ `payload` e `aad` uguali.** È la condizione della
  deduplica, ed è anche quel che rende non esprimibile il difetto v1/v2.
- **L'etichetta è derivata da `envelopeVersion`, non scritta a mano.**
  Incrementare la versione senza cambiare la derivazione non è
  rappresentabile. Regola fissata da
  `TestTheNonceLabelFollowsTheEnvelopeVersion` (`pkg/crypt`).

Test che coprono la §5.3:
`TestTheConvergentNonceCoversEveryAuthenticatedField` (versione, codec, flag e
ruolo cambiano il nonce), `TestConvergentNonceIsSealedPayloadDerived`,
`TestConvergentNonceIsRoleSeparated`, `TestConvergentBlobsStillDeduplicate`
(la dedup è intatta), `TestNoncesNeverRepeat` (modalità normale).

---

## 6. `chunkIndex`: perché è fuori dal nonce convergente

In modalità normale `chunkIndex` è nell'AAD e un chunk spostato di posizione è
rifiutato con `ErrIntegrity` (exit code 5).

In modalità convergente i quattro byte sono zero, **deliberatamente**: un
confine CDC può spostare lo stesso chunk a un indice diverso nel backup
successivo, e legare l'indice renderebbe diversi due blob altrimenti
identici, cioè annullerebbe la deduplica fra tag — l'intero scopo di
`--dedup`. Per la stessa ragione l'AAD non lega il blob a un backup: un
identificatore di backup nell'AAD produrrebbe blob diversi per chunk uguali.

Cosa porta l'integrità posizionale al posto suo:

1. Il blob privato sigillato contiene, per ogni chunk, il **digest del
   plaintext** e la dimensione in chiaro (`ps`, `pb` in `chunks.json` dopo
   `MergePrivate`). Su un backup cifrato il restore li verifica **sempre**: la
   verifica non è disattivabile da nessun flag (`--no-verify` non la tocca).
2. La verifica avviene **prima** della scrittura. `StreamTar` decomprime due
   volte: la prima passata calcola il digest e non consegna nulla, la seconda
   scrive solo se la prima è passata (fase A3.1). Un chunk spostato non emette
   byte prima di essere rifiutato.
3. Dalla 0.5.0 il blob privato lega anche i file di metadati fra loro (§10),
   quindi la tabella dei chunk che porta quei digest non è sostituibile.

Un chunk trapiantato fra due backup che condividono la chiave apre quindi
correttamente a livello GCM — è la conseguenza voluta della convergenza — e
viene respinto un livello più in alto, sul digest del plaintext atteso in
quella posizione. Test: `TestASealedBlobDoesNotTravelBetweenBackups`
(`pkg/recovery`), la modalità `-swap` di `test/e2e/tools/forgeclear` e
`test/e2e/phase_A3.sh`.

**Domanda esplicita al revisore**: questo è il punto in cui la deduplica compra
una proprietà (blob condivisibili) pagando con un'altra (l'AAD non individua
la posizione né il backup). La compensazione è a un livello superiore e
riguarda i dati consegnati, non l'autenticazione del singolo blob.

---

## 7. Gestione delle chiavi e rotazione (0.5.0)

### 7.1 Il problema chiuso

Fino alla 0.4.0 la decisione «questa chiave può sigillare di nuovo?» veniva da
`manifest.json`, cioè da un file **pubblico e non autenticato** che chiunque
possa pubblicare un tag nel repository può riscrivere. Dichiarare
`encryption.envelopeVersion: 2` su un backup vecchio bastava a far tornare al
lavoro una chiave la cui GHASH poteva già essere compromessa, con il file age
intatto.

### 7.2 La regola attuale

L'attestazione sta **dentro** l'involucro age, quindi modificarla richiede
l'identità che lo apre — e chi ce l'ha ha già la DEK. La decisione è presa
solo da lì (`KeyMaterial.ReusableFor`, `pkg/crypt/key.go`):

| Condizione | Esito |
| --- | --- |
| `schemaVersion < 2` (nessuna attestazione) | `ErrKeyNotAttested` |
| `envelopeVersion != 3` (in **entrambe** le direzioni) | `ErrKeyEpoch` |
| `nonceMode` diverso da quello della corsa | `ErrKeyNonceMode` |
| `reuse != "convergent-dedup"` | `ErrKeyNotReusable` |
| altrimenti | riuso consentito |

L'uguaglianza esatta dell'epoca vale nei due versi: materiale di un envelope
più vecchio può avere già sigillato due byte diversi sotto un nonce; materiale
di uno più nuovo verrebbe trascinato in una derivazione per cui non è stato
fatto.

Il campo pubblico `encryption.envelopeVersion` resta nel manifest come
**suggerimento** per pianificare una corsa prima di aprire alcunché, mai come
autorità. Un manifest che mente non riabilita una chiave bruciata né brucia
una chiave sana: entrambe le direzioni sono fissate da
`TestOnlyTheAttestationDecidesReuse` e
`TestALyingManifestCannotBurnAGoodKeyEither` (`pkg/backup/dedup_key_test.go`),
e provate dalla CLI in `test/e2e/phase_A6.sh`.

### 7.3 Materiale generato, per tipo di corsa

| Corsa | Costruttore | `nonceMode` | `reuse` |
| --- | --- | --- | --- |
| cifrata senza `--dedup` | `NewKeyMaterial` | `random` | `never` |
| cifrata con `--dedup` | `NewDedupKeyMaterial` | `convergent` | `convergent-dedup` |

Una chiave nata per una corsa normale non è mai riusata: non è una restrizione
prudenziale, è la sua politica dichiarata.

### 7.4 Rotazione

`backimage backup --dedup --rotate-key` genera materiale nuovo anche quando il
precedente sarebbe riusabile. È una decisione che si scrive, e il costo è
annunciato sull'output: **un ricaricamento completo, una volta sola**. Dal
backup successivo la deduplica riprende normalmente. Misura in
`TestRotationCostsOneFullReupload` (`pkg/backup`) e in
`test/e2e/phase_A6.sh`, che confronta l'impronta della chiave prima e dopo.

Lo stesso costo si paga, senza chiederlo, quando l'attestazione rifiuta il
riuso: il messaggio nomina il motivo e il costo.

---

## 8. Deduplica: cosa garantisce e cosa perde

`--dedup` è **opt-in** e non è attivo di default.

**Garantisce**: chunk identici sotto la stessa chiave producono blob
memorizzati identici, quindi condivisibili fra tag dello stesso repository; il
registry li conserva una volta sola.

**Rivela**, a chi può osservare il registry (non serve la chiave):

- quali chunk e quali layer sono condivisi fra due backup, e quindi una stima
  di **quanto** i dati sono cambiati fra due esecuzioni;
- l'uguaglianza di due chunk, che con contenuti scelti dall'attaccante diventa
  un oracolo di uguaglianza sul plaintext.

**Non rivela**: il plaintext, la DEK, i nomi dei file (l'indice è sigillato),
le dimensioni in chiaro (viaggiano nel blob privato).

**Raccomandazione operativa**, ripetuta in [security.md](security.md): non
usare `--dedup` per dati ad alto rischio di analisi delle modifiche, o con un
avversario che possa scegliere contenuti noti e osservare il registry.

**Vincolo di determinismo**: la deduplica poggia sul fatto che il compressore
produca gli stessi byte per gli stessi input. Il numero di worker di zstd non
li cambia — misurato su 48 combinazioni di dimensione, livello e worker, e
bloccato da `TestZstdOutputIndependentOfWorkerCount` (`pkg/compress`) — ma un
aggiornamento della libreria può cambiarli, e in tal caso la dedup degrada
(più upload) senza conseguenze di sicurezza: dalla v2 in poi il nonce segue i
byte cifrati.

---

## 9. Bilancio delle collisioni

**Modalità normale.** Nonce da 96 bit estratti a caso, uno per blob. Limite di
compleanno: con `n` blob sotto una DEK la probabilità di una collisione è
≈ `n²/2^97`. Con `n = 2^32` (4 miliardi di chunk, ≈ 4 EiB a 1 MiB per chunk)
vale ≈ 2⁻³³. Il consiglio pubblicato è di restare ampiamente sotto quel
numero e, oltre, generare un nuovo backup con una nuova DEK.

**Modalità convergente.** Il nonce è `HMAC-SHA256(...)[0:12]`. Ingressi
distinti che collidono su 96 bit sono una collisione di nonce **con messaggi
diversi**, cioè lo scenario catastrofico della §5.2. Assumendo l'HMAC un PRF,
il bilancio è lo stesso: ≈ `n²/2^97` per `n` blob **distinti** sotto una
chiave. I blob non distinti collidono per costruzione, ed è la deduplica.

Da valutare da parte del revisore: la troncatura a 96 bit di un HMAC-SHA256 e
il fatto che, in un repository con chiave riusata, `n` cresce su **tutti** i
backup che condividono quella chiave, non su uno solo.

**Limite per messaggio.** AES-GCM ammette ≈ 64 GiB di plaintext per messaggio.
Un chunk è al massimo `chunk.MaxChunkSize` = 1 GiB (`pkg/chunk/split.go`), i
blob di metadati sono più piccoli di così: il limite non è raggiungibile.

---

## 10. Legame fra i file di metadati (0.5.0)

Non è crittografia nuova, ma cambia cosa un attaccante può comporre, quindi
appartiene al perimetro della review.

`manifest.json` e `chunks.json` sono pubblici e non hanno firma propria.
L'unico controllo incrociato esistito era che i conteggi dei chunk
coincidessero: si poteva servire il manifest di un backup con la tabella dei
chunk di un altro, o con un indice più vecchio della stessa chiave, e ogni
blob autenticava comunque perché la chiave era la stessa.

Il blob privato è sigillato, quindi quel che dice degli altri file non è
modificabile senza la chiave. Ora li nomina tutti (`pkg/index/binding.go`):

```json
{
  "manifest": "sha256:…",   // digest canonico del manifest pubblico
  "chunks":   "sha256:…",   // digest canonico della tabella dei chunk
  "index":    "sha256:…",   // digest del blob indice memorizzato
  "policy": { "schema": 2, "aead": "aes256-gcm",
              "envelopeVersion": 3, "nonceMode": "convergent",
              "indexEncrypted": true }
}
```

Il lettore lo verifica subito dopo `Unlock`, **politica prima dei digest**, e
prima che un byte raggiunga un parser tar. Ogni scarto esce con codice 5.

Due dettagli che un revisore chiederebbe:

- Il digest canonico del manifest **esclude** il riferimento al blob privato
  (il manifest nomina il digest del blob privato, che nomina il digest del
  manifest: il ciclo va rotto) e i campi che `MergePrivate` ripristina
  (sorgenti, host, totali, impronta della chiave, destinatari), perché il
  lettore verifica prima del merge e lo scrittore calcola dopo lo split. Nessuno
  di questi resta scoperto: sono contenuto autenticato del blob privato.
- Un blob privato **senza** legame è rifiutato quando il materiale di chiave
  attesta l'envelope corrente. Entrambi i fatti vengono da fonti autenticate,
  quindi servire un blob privato più vecchio non toglie la proprietà. Per i
  backup non cifrati la proprietà non esiste e viene dichiarata tale: non c'è
  un file autenticato in cui metterla.

---

## 11. Vettori golden e fixture

**Vettore convergente**, ingressi fissi (DEK
`00112233…eeff` ripetuta, NonceKey `ffeeddcc…1100` ripetuta, codec `store`,
`role = data`, `chunkIndex = 0`, payload `"vector"`), in
`pkg/crypt/golden_test.go`:

| Envelope | Blob completo (hex) |
| --- | --- |
| v3 (scritto oggi) | `42494d4743484b3103000101a4e94ec22c8a2a2a83cb0d5d506c800dd063410c896a8685269b00a21e57c0fec1f7` |
| v2 (0.2.4 – 0.4.0) | `42494d4743484b3102000101f1d50e175143ea9cd2111471cf9e115b36cb947c49afae148e172e6b969baa973d26` |

Il primo è riprodotto dal writer a ogni corsa dei test; il secondo è solo
**letto**, come prova che un backup pubblicato continua ad aprirsi. Rompere il
primo intenzionalmente significa incrementare `envelopeVersion` e aggiornare
questo documento.

**Fixture di formato**, in `pkg/recovery/testdata/`, un backup completo per
ogni formato rilasciato:

| Fixture | Schema metadati | Envelope dei blob | Note |
| --- | --- | --- | --- |
| `schema1-plain` | 1 | nessuno | non cifrato: i blob sono flussi compressi puri |
| `schema2-encrypted` | 2 | 2 | nonce casuale, nessun legame dei metadati |
| `schema2-envelope3` | 2 | 3 | il formato che questo albero scrive, con il legame |
| `legacy-envelope1` | 2 | 1 | prodotta dal tag `v0.2.3-dev.3`; il manifest non porta ancora il campo |

Sono **evidenza congelata**: `scripts/make-format-fixtures.sh` rifiuta di
sovrascriverne una esistente e richiede `--force`, perché rigenerarla cancella
la prova su cui poggiano i test di compatibilità
(`pkg/recovery/format_compat_test.go`). La passphrase delle fixture cifrate è
`fixture-passphrase`. `test/e2e/phase_A6.sh` le apre anche con l'autoestraente
che questo albero incorpora, non solo dai test Go.

---

## 12. Rischio residuo

Questo documento **non chiude** la voce «review crittografica indipendente
della modalità convergente». Resta dichiarata fino a quando un revisore
esterno non si esprime sulle domande della §1. Le correzioni descritte qui
riducono la superficie che quella review deve esaminare; non la sostituiscono.
