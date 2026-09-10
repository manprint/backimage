# Fase A6 — Formato autenticato: un solo bump

**Obiettivo**: ciò che descrive il backup — epoca crittografica, politica di cifratura, legame fra
i file di metadati — sta dentro il materiale autenticato, non in campi pubblici che chiunque può
riscrivere.

**Rilievi coperti**: A05, A19, A20.

**Perché insieme**: sono tre facce dello stesso problema e richiedono lo stesso incremento di
`envelopeVersion`/schema. Farli in tre momenti diversi significa tre migrazioni e tre finestre di
incompatibilità.

**Precondizione**: A1 chiusa. L'opener stretto è il presupposto del binding: senza di esso il
lettore validerebbe un legame che poi accetta di leggere in chiaro.

---

## A6.0 Congelare le fixture dei formati attuali — **da fare per primo**

**Agente: Sonnet**

Scoperto nel ricontrollo: `pkg/recovery/testdata` **non esiste**, e i test costruiscono le fixture
in codice (`pkg/recovery/recovery_test.go:136` e seguenti, `buildFixture`). In `pkg/index/testdata`
ci sono solo i golden del formato corrente (`manifest.golden.json`, `chunks.golden.json`,
`index.golden.zst`); in `pkg/crypt/testdata` solo `keys.age`.

Conseguenza pratica: **appena il writer cambia, non esiste più un modo di produrre un backup del
formato vecchio**, e i test di lettura retrocompatibile diventano impossibili da scrivere.

Quindi, prima di toccare qualunque cosa in questa fase:

- Generare e committare fixture complete di backup nei formati già rilasciati: schema 1 non
  cifrato, schema 2 cifrato con `envelopeVersion` corrente, e schema 2 con `envelopeVersion`
  precedente se ottenibile da un tag (`git worktree` su `v0.2.2`, build, generazione).
- Fixture piccole ma complete: manifest, chunks, index, private, keyfile age, layer.
- Un test di lettura per ciascuna, che gira **prima** e **dopo** il cambio di formato con lo stesso
  esito.

Senza questa sub-fase, i criteri di accettazione di A6.1 e A6.3 non sono verificabili.

## A6.1 Epoca e politica dentro il materiale avvolto

**Agente: Opus** (progetto) + **Sonnet** (implementazione). **Rilievo**: A05.

### Il difetto, verificato

`pkg/backup/pipeline.go:703-705`:

```go
func legacyEnvelopeKey(previous *dedupBase) bool {
	return previous.manifest.Encryption.EnvelopeVersion < crypt.EnvelopeVersion
}
```

La decisione se una chiave è "bruciata" dipende **esclusivamente** da un campo pubblico del
manifest. Il commento a `pkg/index/model.go:99-106` dichiara la scelta come deliberata: il campo è
pubblico perché una run con `--dedup` deve poter decidere prima di scartare qualunque cosa. Il
materiale avvolto da age contiene schema, DEK e NonceKey, ma **non** l'epoca di creazione.

Riproduzione della review: lo stesso blob age viene rifiutato quando il manifest dichiara versione
1, e restituisce esattamente la stessa DEK quando si cambia solo il campo pubblico.

### Dove intervenire, concretamente

`pkg/crypt/key.go:14-21` è il punto esatto:

```go
const schemaVersion = 1

type KeyMaterial struct {
	SchemaVersion int    `json:"schemaVersion"`
	DEK           []byte `json:"dek"`      // 32 bytes, base64 in JSON
	NonceKey      []byte `json:"nonceKey"` // 32 bytes, used only in convergent mode
}
```

Il materiale è JSON, serializzato e avvolto da age: aggiungere campi è compatibile in avanti e
autenticato dal wrapping. Servono `envelopeVersion` (l'epoca crittografica di creazione),
`nonceMode` e la politica di riuso; `schemaVersion` passa a 2. Un materiale con
`schemaVersion == 1` è per definizione **senza attestazione**, e ricade nella regola sotto.

### Intervento

- Includere dentro il materiale **autenticato e avvolto**: epoca/versione crittografica di
  creazione, schema, modalità nonce, politica di riuso.
- La decisione di riuso si prende da quei valori, non dal manifest. Il campo pubblico resta come
  suggerimento per la fase di pianificazione, mai come autorità.
- Formato vecchio, privo di attestazione: **generare una chiave nuova prima di scrivere**. Non
  inferire l'epoca da header o tag pubblici, e non riusare una chiave legacy solo perché age la
  apre.
- Conservare un ancoraggio fidato della chiave di repository e una decisione **esplicita** di
  rotazione.

**Costo per l'utente**: il primo backup dopo l'aggiornamento su un repository con chiave legacy
ricarica i dati per intero, una volta. Va misurato e dichiarato nel changelog assieme alla dedup
successiva, che torna normale.

## A6.2 Nonce convergente separato su tutti i campi autenticati

**Agente: Opus** (progetto) + **Sonnet** (implementazione). **Rilievo**: A19.

### Il difetto, verificato

`pkg/crypt/chunk.go:125-132`:

```go
func convergentNonce(nonceKey []byte, role Role, payload []byte) []byte {
	sum := sha256.Sum256(payload)
	mac := hmac.New(sha256.New, nonceKey)
	mac.Write([]byte(nonceLabel))
	mac.Write([]byte{byte(role)})
	mac.Write(sum[:])
	return mac.Sum(nil)[:nonceLen]
}
```

Il nonce copre `role` e `payload`. L'AAD invece copre l'header intero — `Version`, `Codec`,
`Flags`, `AEAD` — più `role` e `chunkIndex`: `AAD(h, role, chunkIndex)` compare in `Seal`
(`chunk.go:177`) e in `Open` (`chunk.go:198`).

Conseguenza: due blob con payload e ruolo identici ma header differente ricevono **lo stesso
nonce** e AAD differenti. Due messaggi GCM distinti sotto la stessa coppia (chiave, nonce)
permettono di risolvere GHASH per la chiave di autenticazione, cioè di falsificare tag arbitrari
sotto quella DEK.

`Codec` non è l'innesco realistico: un codec differente produce byte compressi differenti, quindi
nonce differente. **L'innesco è `Version`**: `nonceLabel` è la costante `"backimage/nonce/v2\x00"`
(`chunk.go:29`) mentre `envelopeVersion` è indipendente. Il giorno in cui si incrementa
`envelopeVersion` senza toccare `nonceLabel`, su un repository con chiave riusata e `--dedup`, la
collisione è sistematica. Ed è esattamente ciò che questa fase sta per fare.

### Intervento

- Derivare il nonce anche dai campi autenticati dell'header, non solo da ruolo e payload.
- Rendere la regola strutturale, non documentale: `nonceLabel` deve derivare da `envelopeVersion`,
  in modo che sia impossibile incrementare la versione senza cambiare la derivazione. Un test lo
  impone confrontando i due valori.
- Costo sulla deduplicazione: **nullo**. A parità di configurazione i campi dell'header sono
  costanti, quindi payload identici continuano a produrre nonce identici.
- Aggiornare il commento di `convergentNonce`, che oggi racconta correttamente l'incidente
  pre-0.2.4 ma non questo.

## A6.3 Legame autenticato fra manifest, chunks, index, private e politica

**Agente: Opus** (progetto) + **Sonnet** (implementazione). **Rilievo**: A20.

### Il difetto, verificato

L'unico controllo incrociato fra i file di metadati è `pkg/recovery/recovery.go:144` (e il gemello
alla riga 113):

```go
if m.Chunking.Count != len(t.Chunks) {
	return nil, fmt.Errorf("%w: manifest has %d chunks, table has %d", ...)
}
```

Non esiste alcun MAC o firma che leghi `manifest.json`, `chunks.json`, l'indice, il blob private e
la politica di cifratura attesa. Nulla impedisce di comporre pezzi provenienti da backup diversi,
purché i conteggi tornino.

### Intervento

- Il blob **private**, che è già autenticato, porta: digest di manifest e chunks, digest
  dell'indice, e la politica attesa (`aead`, `schema`, `envelopeVersion`, `nonceMode`, presenza dei
  blob obbligatori).
- Il lettore li verifica **subito dopo `Unlock`**, prima di consegnare dati a un parser tar o allo
  stdout.
- Backup non cifrati: non hanno un blob autenticato dove mettere il legame. La proprietà va
  dichiarata come non disponibile per schema 1, senza fingere il contrario.
- Ordine di validazione: politica → legame → dati. Un errore in qualunque punto è di classe
  integrità/formato.

## A6.4 Dossier per la review crittografica indipendente

**Agente: Opus**

La review dichiara che non sostituisce una review crittografica indipendente della modalità
convergente. **Questa fase non può colmare quel punto**: quello che può fare è renderlo
affrontabile da un revisore esterno.

Documento in `docs/` con: derivazione del nonce (prima e dopo A6.2), composizione dell'AAD, ruolo
di `chunkIndex` e perché è deliberatamente fuori dal nonce in modalità convergente, garanzie e
perdite dichiarate della deduplicazione, gestione delle chiavi e rotazione dopo A6.1, vettori
golden (`pkg/crypt/testdata`, `pkg/crypt/golden_test.go`).

**Resta rischio residuo dichiarato** in `overview.md` fino a quando una review esterna non lo
chiude.

---

## Accettazione della fase

Per A05, dalla review: manipolare versione, `nonceMode`, timestamp e scelta del tag base **senza
cambiare il file age**; nessuna di queste modifiche deve riabilitare una chiave bruciata. Misurare
il costo del reupload una tantum e verificare che la dedup successiva funzioni.

Per A19: test che fallisce se `nonceLabel` non cambia insieme a `envelopeVersion`; verifica che
payload identici sotto la stessa configurazione continuino a deduplicare.

Per A20: comporre un backup con manifest, chunks, index e private provenienti da backup diversi
sotto la stessa chiave, con conteggi coerenti: deve essere rifiutato prima di qualunque emissione.

**Migrazione**: i backup esistenti restano leggibili. Il nuovo formato è scritto solo dai backup
nuovi. Il test di lettura per ciascuna combinazione schema/envelope già rilasciata usa le fixture
congelate in A6.0 — che vanno prodotte **prima** di toccare il writer.

**e2e**: `test/e2e/phase_A6.sh` — rotazione con chiave legacy, composizione ostile dei metadati,
roundtrip su formato nuovo e lettura di fixture dei formati vecchi.

**Documentazione**: `docs/image-format.md` (il formato cambia), `docs/dedup.md` (il costo del
reupload e la dedup successiva), `docs/security.md`, `docs/ARCHITECTURE.md`, il dossier
crittografico, `CHANGELOG.md`.

---

## Uscita di fase

- Nessuna decisione di sicurezza dipende da un campo pubblico riscrivibile.
- La derivazione del nonce non può collidere per effetto di un bump di versione.
- I metadati di un backup non si possono ricombinare fra backup diversi.
