# Modelo di sicurezza

Versione: 3 · Aggiornato: 0.4.1 · Applicabile a: envelope `BIMGCHK1` v3, keyfile age (schema 2, attestato), CLI (`--dedup`, `--rotate-key`, `genpass`).

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
| DEK (256 bit) | `crypto/rand`, una per backup; riusata solo da `--dedup` quando il **materiale avvolto** attesta la stessa epoca dell'envelope e la modalità convergente | AES-256-GCM per ogni chunk |
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

Il layout dei byte è identico in tutte e tre le versioni: cambiano solo la
derivazione del nonce convergente e la forma dei dati autenticati. Le versioni
1 e 2 continuano a essere **lette** (i backup già pubblicati si ripristinano
intatti) e non vengono mai più **scritte**; la 3 è quella che questa release
scrive. Un `backimage` precedente rifiuta un blob nuovo con
`unsupported blob version 3 (support 1-2)`.

### Nonce (limiti GCM — CRITICO)

- Modalità default: 12 byte da `crypto/rand`, **mai riutilizzato** (verificato da
  test `TestNoncesNeverRepeat`). AES-GCM con chiave singola: limite pratico
  ≈ 2³² chunk cifrati con la stessa DEK; con 1 MiB/chunk → ≈ 4 EiB. Al di là
  ri-generare un nuovo backup (nuova DEK).
- Modalità convergente (`--dedup`, opt-in), envelope v3:
  `nonce = HMAC-SHA256(NonceKey, "backimage/nonce/v3\0" ‖ AAD ‖ sha256(payload_sigillato))[0:12]`,
  dove `AAD` è il blocco autenticato descritto sotto (magic, versione, codec,
  aead, flag, ruolo; l'indice del chunk non c'è in modalità convergente).
  Il digest è quello dei byte che GCM cifra davvero, cioè il chunk **già
  compresso**. Per chunk identici con la stessa chiave → nonce e ciphertext
  identici → dedup. La chiave HMAC impedisce di ricavare il nonce da un
  dizionario pubblico. Questa modalità rivela comunque l'uguaglianza dei chunk a
  chi osserva il registry.
- No riutilizzo nonce tra modalità: il client riusa una `KeyMaterial` solo se
  **quel materiale lo dichiara di sé**. Da 0.4.1 il JSON avvolto da age porta
  `envelopeVersion`, `nonceMode` e `reuse`, ed è l'unica autorità sul riuso; da
  `random` a `convergent`, da un'altra epoca dell'envelope, o da una chiave
  senza attestazione, genera sempre una nuova chiave. GCM con nonce ripetuti
  sotto la stessa DEK sarebbe una perdita totale di confidenzialità.
- Perché non basta il manifest: `manifest.json` non è autenticato, e chiunque
  possa pubblicare un tag nel repository può riscriverne i campi. Fino alla
  0.4.0 la decisione dipendeva solo da `encryption.envelopeVersion`, quindi
  dichiarare `2` su un backup vecchio bastava a far tornare al lavoro una
  chiave bruciata, con lo stesso file age intatto. L'attestazione sta dentro
  l'involucro age: modificarla richiede l'identità che lo apre, e a quel punto
  l'attaccante ha già la DEK. Il campo pubblico resta come suggerimento per
  pianificare la corsa prima di aprire alcunché.
- Rotazione esplicita: `backimage backup --dedup --rotate-key` genera comunque
  materiale nuovo. Il costo è un ricaricamento completo, una volta sola,
  annunciato sull'output e misurato in [dedup.md](dedup.md).

I blob di metadati (`index.json.zst`, `private.json.zst`) sono sigillati con lo
stesso schema e con un `role` distinto, quindi il nonce dipende dal contenuto e
dal tipo di blob.

#### Perché il nonce copre i dati autenticati (0.4.1)

Fino alla 0.4.0 il nonce convergente derivava da ruolo e payload, mentre l'AAD
copriva l'header intero. Due blob con lo stesso payload e header diverso
ricevevano quindi **lo stesso nonce** e AAD diversi: due messaggi GCM sotto la
stessa coppia (chiave, nonce), cioè di nuovo il recupero della chiave GHASH.
`Codec` non era l'innesco realistico — un codec diverso produce byte diversi,
quindi nonce diverso — ma `Version` sì: l'etichetta di dominio era la costante
`"backimage/nonce/v2\0"`, indipendente da `envelopeVersion`, e il giorno in cui
la versione fosse cambiata senza toccarla, su un repository con chiave riusata
e `--dedup`, la collisione sarebbe stata sistematica.

Correzione in due parti. Il nonce è derivato dall'AAD, quindi da tutti i campi
autenticati; e l'etichetta è **derivata** da `envelopeVersion`
(`"backimage/nonce/v" + envelopeVersion + "\0"`), così incrementare la versione
senza cambiare la derivazione non è esprimibile. La regola è fissata da
`TestTheNonceLabelFollowsTheEnvelopeVersion` in `pkg/crypt`.

Costo sulla deduplica: nullo. A parità di configurazione i campi dell'header
sono costanti, quindi payload identici continuano a produrre nonce identici;
`TestConvergentBlobsStillDeduplicate` lo fissa. Fra epoche diverse invece i
blob cambiano, ed è il motivo per cui una chiave non attraversa un bump di
`envelopeVersion`.

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

Envelope v2 e v3, per ogni blob:
`magic(8) | version | codec | aead | flags | role | uint32be(chunkIndex)` (17 byte).
Il byte `version` è dentro l'AAD, e da v3 l'AAD è anche l'ingresso della
derivazione del nonce: un bump di versione sposta insieme dati autenticati e
nonce.

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

### Un backup cifrato non ha blob in chiaro (0.4.1)

L'envelope dichiara nel proprio header quale AEAD lo protegge, e `aead=none` è
un valore legittimo: è la forma che assume un backup **non** cifrato. Fino alla
0.4.0 esisteva un solo lettore per entrambe le forme, e restituiva il payload di
un blob `aead=none` anche quando possedeva la chiave. Riscrivere l'header di un
blob è alla portata di chiunque possa riscrivere il blob — non serve alcuna
chiave — quindi il percorso di lettura cifrato poteva essere convinto a
consegnare byte che nessuno aveva autenticato: dati, indice dei file o metadati
riservati, singolarmente o insieme.

Dalla 0.4.1 i due lettori sono tipi distinti e la scelta fra loro si fa **una
volta sola**, da `encryption.enabled` nel manifesto, mai dall'header del blob
che si sta per leggere:

| Lettore | Backup | `aead=none` | `aead=aes256-gcm` |
| --- | --- | --- | --- |
| `crypt.NewKeyedOpener` | cifrato | **rifiutato**, errore di integrità | aperto e verificato |
| `crypt.NewClearOpener` | in chiaro | letto | rifiutato, «key material required» |

Conseguenze dirette:

- `index.ReadIndex` riceve l'aspettativa come parametro invece di dedurla dalla
  forma del blob: dentro un backup cifrato un indice senza envelope, o con un
  envelope non autenticato, è un errore di formato e non una versione vecchia.
- `index.ReadPrivate` richiede un lettore con chiave. Il magic dell'envelope
  dice «blob backimage», non «blob autenticato»: da solo lasciava passare un
  `aead=none`.
- Un manifesto che dichiara cifratura e non porta il blob privato dello schema
  2 — o che dichiara di non essere cifrato e ne porta uno — viene respinto
  prima che qualunque blob venga toccato.
- Il rifiuto precede **sempre** l'emissione: nessun byte di plaintext raggiunge
  tar, stdout o filesystem.
- Il rifiuto esce con **codice 5, integrità**, su entrambi gli eseguibili. Un
  blob privato non autenticato faceva fallire lo sblocco e usciva 4, «passphrase
  errata»: mandava a cercare un errore di battitura mentre la risposta onesta
  era che l'immagine non è più quella che dichiara di essere.

Regressioni: `TestKeyedOpenerRejectsUnauthenticatedBlob` e
`TestClearOpenerRejectsEncryptedBlob` (`pkg/crypt`),
`TestReadIndexOfEncryptedBackupRejectsAClearEnvelope` e
`TestReadPrivateRejectsAClearEnvelope` (`pkg/index`),
`TestEncryptedBackupRefusesDowngradedBlobs` (`pkg/recovery`), che falsifica
dati, indice e blob privato singolarmente e insieme e verifica su ogni comando
di lettura che l'errore arrivi con zero byte scritti,
`TestUnlockErrorSeparatesTamperingFromACredential` (`internal/cli`) e
`TestUnlockErrorMatchesTheHostClassification`
(`cmd/backimage-selfextract`) per la classificazione.

End-to-end: `test/e2e/phase_A1.sh` costruisce un backup cifrato reale, ne
riscrive dati, indice e blob privato come envelope in chiaro **riparando ogni
numero pubblico che li descrive** — dimensioni, digest memorizzati, nomi dei
blob, metadati dei layer — e li ripubblica come layout OCI e come immagine su
registry. Nulla di verificabile senza la chiave resta incoerente: ciò che
rifiuta il backup è solo la regola sull'AEAD. Lo script verifica il rifiuto sul
binario host e sull'estrattore, per ogni comando di lettura, e verifica anche
che le superfici ancora autenticate continuino a rispondere.

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

### `--expect-digest`: ancorare la lettura a un valore esterno (0.4.1)

`restore`, `verify`, `ls`, `find` e `inspect` del **binario host** accettano
`--expect-digest sha256:…`. Se l'immagine che il riferimento risolve non ha
quel digest, il comando esce con **codice 5 (integrità) prima di leggere la
passphrase o il file di identità**: la credenziale non viene consegnata a
un'immagine che non è quella richiesta.

Cosa viene confrontato, per sorgente:

| Sorgente | Valore letto dalla sorgente |
| --- | --- |
| registry | il digest che il registry associa al riferimento (`remote.Get`), cioè l'indice multi-arch per un tag multi-piattaforma |
| `--oci-layout` | il digest dell'indice della layout — lo stesso che `backimage backup` stampa nel campo `digest` — oppure quello di uno dei manifest che l'indice pubblica |
| `--local-repo` (daemon) | il digest che l'immagine ha **nel daemon**, che non è quello che aveva nel registry: il daemon ricomprime i layer |

**Il flag vale quanto vale il canale da cui arriva il digest.** Un digest letto
dalla stessa sorgente che fornisce l'immagine non prova niente: chi può
sostituire l'immagine può sostituire anche il digest che la accompagna. Va
ottenuto altrove — una nota di rilascio firmata, un ticket, la macchina che ha
eseguito il backup — e confrontato qui. Per la stessa ragione il flag **non
esiste nell'autoestraente**: dentro l'immagine non c'è nulla da confrontare che
l'autore dell'immagine non controlli già.

```console
# sulla macchina che ha eseguito il backup
$ backimage backup /srv --repo ghcr.io/me/dumps --tag daily --json | jq -r .digest
sha256:9f2c…

# altrove, con quel valore arrivato per un canale diverso dall'immagine
$ backimage restore ghcr.io/me/dumps:daily --expect-digest sha256:9f2c… \
    -x -C ./out --passphrase-file ./pass
```

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

La mappatura completa è in `internal/cli/errors.go` (`Kind` → codice):

| Codice | Significato |
|---|---|
| 0 | successo |
| 1 | errore generico (`KindGeneric`) |
| 2 | errore d'uso (`KindUsage`) |
| 3 | privilegi insufficienti (`KindPermission`) |
| 4 | passphrase mancante/sbagliata (`KindPassphrase`, `ErrPassphrase`/`ErrWrongPassphrase`) |
| 5 | integrità fallita (`KindIntegrity`, `ErrIntegrity`) — dati manomessi |
| 6 | errore di rete o del registry (`KindNetwork`) |
| 7 | interrotto (`KindInterrupted`) |

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
