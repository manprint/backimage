# Fase 03 — `pkg/crypt`: cifratura

**Obiettivo**: cifratura **attiva di default** (D06), disattivabile con `--no-encrypt`. DEK casuale, AES-256-GCM per chunk, DEK incapsulata con **age** (passphrase scrypt e/o destinatari X25519).

**Vincolo speciale**: questo pacchetto è importato anche dal binario self-extract (fase 06), che ha un budget di 8 MB e non può importare cobra né ggcr. `pkg/crypt` deve dipendere solo da stdlib + `filippo.io/age` + `golang.org/x/term`.

**Riferimento decisioni**: D06, D15 (nonce convergente predisposto ma non attivo).

> **Regola di sicurezza per l'implementatore**: non inventare primitive, non scrivere KDF a mano, non riusare mai un nonce, non stampare mai una chiave o una passphrase in un log o in un messaggio di errore. Ogni scostamento da questa fase va escalato a Opus, mai deciso in autonomia.

---

## 03.1 `KeyMaterial`, generazione DEK, zeroizzazione

**Agente: Sonnet**

### File: `pkg/crypt/doc.go`

Deve dichiarare in modo esplicito: ordine `tar → compressione → cifratura`; formato dell'envelope; che senza passphrase o chiave privata il backup è **irrecuperabile**; che il nonce convergente (fase 10) indebolisce la riservatezza in modo noto e documentato.

### File: `pkg/crypt/key.go`

```go
// KeyMaterial holds the secrets of one backup. It must be wiped after use.
type KeyMaterial struct {
	SchemaVersion int      `json:"schemaVersion"`
	DEK           []byte   `json:"dek"`      // 32 bytes, base64 in JSON
	NonceKey      []byte   `json:"nonceKey"` // 32 bytes, used only in convergent mode
}

// NewKeyMaterial generates fresh random secrets using crypto/rand.
func NewKeyMaterial() (*KeyMaterial, error)

// Wipe overwrites the secrets in place. Safe on a nil receiver.
func (k *KeyMaterial) Wipe()

// Validate checks lengths and schema version.
func (k *KeyMaterial) Validate() error
```

### Prescrizioni
- `crypto/rand.Read` esclusivamente; errore fatale se fallisce, mai fallback.
- `Wipe` usa un ciclo di azzeramento su `[]byte` seguito da `runtime.KeepAlive(k)`.
- `KeyMaterial` non deve avere metodi `String()`/`Format()`: se qualcuno la stampa, deve venire fuori la struct grezza e non un segreto formattato. Aggiungere invece `func (k *KeyMaterial) GoString() string { return "crypt.KeyMaterial{REDACTED}" }` e `String() string` che restituisce `"crypt.KeyMaterial{REDACTED}"`. **Questa è l'unica forma di `String()` ammessa.**

### Test richiesti
- due chiamate a `NewKeyMaterial` producono DEK diverse;
- `Wipe` azzera davvero (controllo byte per byte);
- `fmt.Sprintf("%v/%s/%#v", k, k, k)` non contiene nessun byte della DEK.

---

## 03.2 Incapsulamento con age

**Agente: Sonnet**

### File: `pkg/crypt/keyfile.go`

```go
// Recipients describes who can open the backup.
type Recipients struct {
	Passphrase []byte   // optional; when set, an age scrypt recipient is added
	AgeKeys    []string // optional; "age1..." X25519 public keys
}

// WrapKeys serialises km as JSON and encrypts it for the given recipients,
// writing an ASCII-armored age file to w.
func WrapKeys(w io.Writer, km *KeyMaterial, rcpt Recipients) error

// UnwrapKeys decrypts an age file produced by WrapKeys.
// identity is either a passphrase or an age private key.
func UnwrapKeys(r io.Reader, id Identity) (*KeyMaterial, error)

// Identity is either a passphrase or an age X25519 identity.
type Identity struct {
	Passphrase []byte
	AgeKeyFile string // path to a file containing "AGE-SECRET-KEY-1..."
}
```

### Prescrizioni
- Passphrase: `age.NewScryptRecipient(string(pass))`; **non** abbassare il work factor di default. Con work factor di default, `UnwrapKeys` impiega ~1 s: accettabile, va detto in doc.
- Almeno un destinatario è obbligatorio: `Recipients` vuoto → errore `"no recipients: refusing to produce an unopenable backup"`.
- **age non permette di mescolare scrypt con altri destinatari** nello stesso file. Se l'utente chiede sia passphrase sia chiavi age, produrre **due file**: `keys.age` (X25519) e `keys.pass.age` (scrypt), entrambi contenenti la stessa `KeyMaterial`. Entrambi vanno inseriti nell'immagine e citati in `manifest.json` sotto `encryption.recipients`. Il self-extract prova prima `keys.age` con la chiave fornita, poi `keys.pass.age` con la passphrase.
- Passphrase errata: `UnwrapKeys` restituisce un errore di tipo riconoscibile:
  ```go
  // ErrWrongPassphrase is returned when no identity can open the key file.
  var ErrWrongPassphrase = errors.New("wrong passphrase or key")
  ```
  che la CLI mappa su exit code **4**.
- Il messaggio d'errore non deve distinguere "passphrase errata" da "file corrotto" in modo che permetta oracoli: un solo messaggio.

### Test richiesti
- wrap con passphrase → unwrap con la stessa passphrase → `KeyMaterial` identica;
- unwrap con passphrase sbagliata → `errors.Is(err, ErrWrongPassphrase)`;
- wrap con chiave age → unwrap con la privata corrispondente;
- wrap con passphrase **e** chiave age → due file, entrambi apribili con la rispettiva identità;
- `Recipients{}` vuoto → errore;
- file troncato a metà → errore, non panic;
- golden file: un `keys.age` committato in `testdata/` con passphrase nota (`testpass`) resta apribile (protezione contro regressioni di formato).

---

## 03.3 Envelope del blob

**Agente: Sonnet**

### File: `pkg/crypt/envelope.go`

Implementare esattamente il formato descritto in `overview.md` §4.4:

```
0   8   magic  "BIMGCHK1"
8   1   version = 1
9   1   codec   (compress.ID)
10  1   aead    (0=none, 1=aes-256-gcm)
11  1   flags   (bit0 = convergent nonce)
12  12  nonce   (presente solo se aead != 0)
24  ..  payload (compresso, poi cifrato) + tag GCM 16B
```

```go
// Header is the fixed prefix of every stored blob.
type Header struct {
	Version uint8
	Codec   compress.ID
	AEAD    uint8
	Flags   uint8
	Nonce   [12]byte
}

// HeaderSize returns the encoded size for the given AEAD setting.
func HeaderSize(aead uint8) int

// MarshalHeader encodes h into dst, which must be at least HeaderSize bytes.
func MarshalHeader(dst []byte, h Header) (int, error)

// ParseHeader decodes a header from src.
func ParseHeader(src []byte) (Header, int, error)

// AAD builds the additional authenticated data for chunk index i.
func AAD(h Header, chunkIndex uint32) []byte
```

### Prescrizioni
- `AAD` = `magic || version || codec || aead || flags || uint32be(chunkIndex)`. Legare il chunk index all'AAD impedisce il riordino dei blob da parte di un attaccante.
- `ParseHeader` su magic errato → errore `"not a backimage blob"`; su version sconosciuta → errore che riporta la versione trovata e quella supportata.
- Nessuna allocazione in `MarshalHeader`.

### Test richiesti
- round-trip header per tutte le combinazioni di codec × aead × flags;
- `ParseHeader` su 24 byte casuali → errore, mai panic;
- `AAD` cambia se cambia il chunk index (test di disuguaglianza);
- fuzz `FuzzParseHeader`, 60 s.

---

## 03.4 Encryptor e Decryptor per chunk

**Agente: Sonnet**

### File: `pkg/crypt/chunk.go`

```go
// NonceMode selects how the per-chunk nonce is derived.
type NonceMode uint8

const (
	// NonceRandom draws 12 random bytes per chunk. Default.
	NonceRandom NonceMode = 0
	// NonceConvergent derives the nonce from the plaintext digest, enabling
	// deduplication at the cost of revealing chunk equality. Phase 10.
	NonceConvergent NonceMode = 1
)

// Sealer encrypts already-compressed chunk payloads.
type Sealer interface {
	// Seal writes the full stored blob (header + ciphertext) for one chunk.
	// plainSHA is the digest of the *plaintext* chunk, used in convergent mode.
	Seal(dst []byte, chunkIndex uint32, codec compress.ID, compressed []byte, plainSHA [32]byte) ([]byte, error)
	// Overhead returns the number of bytes Seal adds to the payload.
	Overhead() int
}

// Opener decrypts stored blobs.
type Opener interface {
	// Open returns the compressed payload of one stored blob.
	Open(dst []byte, chunkIndex uint32, blob []byte) ([]byte, compress.ID, error)
}

// NewSealer builds a Sealer. When km is nil, encryption is disabled and the
// envelope is written with aead=0 (the payload stays in clear).
func NewSealer(km *KeyMaterial, mode NonceMode) (Sealer, error)

// NewOpener builds an Opener. km may be nil only for unencrypted blobs.
func NewOpener(km *KeyMaterial) (Opener, error)

// ErrIntegrity is returned when authentication fails.
var ErrIntegrity = errors.New("blob authentication failed")
```

### Prescrizioni
- `NonceRandom`: 12 byte da `crypto/rand` per ogni chunk.
- `NonceConvergent`: `nonce = HMAC-SHA256(km.NonceKey, plainSHA)[:12]`. Documentare che due chunk con lo stesso plaintext producono lo stesso ciphertext, il che è **il punto** della dedup e **il costo** in riservatezza.
- `Open` con `km == nil` su un blob con `aead != 0` → errore chiaro: *"blob cifrato: fornire la passphrase"*, mappato su exit code 4.
- `Open` con autenticazione fallita → `ErrIntegrity`, exit code 5. **Non** distinguere "chiave sbagliata" da "dati corrotti".
- Riuso dei buffer: `Seal`/`Open` accettano `dst` e lo estendono con `append`. Nessuna allocazione se `dst` ha capienza.
- `cipher.NewGCM` va creato **una volta** nel costruttore, non per chunk.

### Test richiesti
- Seal → Open round-trip su 1000 chunk, contenuti diversi, indici crescenti;
- Open con chunk index sbagliato → `ErrIntegrity` (dimostra che l'AAD funziona);
- flip di un bit in una posizione casuale del blob → `ErrIntegrity` (100 iterazioni);
- `km == nil` (cifratura disattivata) → round-trip funziona, `aead == 0`, e il payload è leggibile in chiaro nel blob;
- modalità convergente: due chunk identici producono blob **byte-identici**; due chunk diversi no;
- modalità random: due chunk identici producono blob **diversi**;
- `AllocsPerRun` di `Seal` con `dst` precaricato ≤ 1;
- test di non-riuso del nonce: 100 000 `Seal` in modalità random, tutti i nonce distinti.

---

## 03.5 Ingresso della passphrase

**Agente: Sonnet**

### File: `pkg/crypt/prompt.go`

```go
// PassphraseSource describes where a passphrase may come from, in priority order.
type PassphraseSource struct {
	Direct   []byte // already provided by the caller (tests only)
	File     string // --passphrase-file
	Stdin    bool   // --passphrase-stdin (reads one line)
	EnvVar   string // default "BACKIMAGE_PASSPHRASE"
	Prompt   bool   // interactive prompt on /dev/tty
	Confirm  bool   // ask twice and compare (backup only)
}

// ReadPassphrase resolves a passphrase according to src.
func ReadPassphrase(src PassphraseSource) ([]byte, error)

// ErrNoPassphrase is returned when no source yields a passphrase.
var ErrNoPassphrase = errors.New("no passphrase available")
```

### Ordine di risoluzione (vincolante)
`Direct` → `File` → `Stdin` → `EnvVar` → `Prompt`. La prima sorgente **configurata** vince; se è configurata e fallisce, si restituisce errore senza provare le successive (comportamento prevedibile negli script).

### Prescrizioni
- Prompt: aprire **`/dev/tty`** (non `os.Stdin`), perché `stdin` può essere occupato da una pipe. Su Windows, `CONIN$`. Se `/dev/tty` non è apribile → `ErrNoPassphrase` con hint che elenca le altre sorgenti.
- Eco disattivato con `golang.org/x/term.ReadPassword`.
- `Confirm`: seconda lettura, confronto con `subtle.ConstantTimeCompare`, messaggio "passphrase non coincidenti" e nuovo tentativo, massimo 3.
- Passphrase vuota → errore. Nessun backup senza segreto quando la cifratura è attiva.
- `File`: rimuovere un solo `\n` o `\r\n` finale, nient'altro (le passphrase possono contenere spazi).
- Documentare in `docs/security.md` che `BACKIMAGE_PASSPHRASE` è visibile in `docker inspect` e in `/proc/<pid>/environ`, e va usato solo in automazione consapevole.

### Test richiesti
- ognuna delle 5 sorgenti, isolata;
- priorità: con `File` e `EnvVar` entrambe configurate vince `File`;
- `File` inesistente → errore, **non** fallback su env;
- `Confirm` con due valori diversi → riprova, e dopo 3 tentativi errore;
- passphrase con spazi e UTF-8 multibyte da file;
- il prompt è testabile grazie a un campo iniettabile `openTTY func() (io.ReadWriteCloser, error)`.

---

## 03.6 Vettori di test, golden file, fuzz

**Agente: Sonnet**

### File: `pkg/crypt/testdata/`
- `keys.age` — wrap con passphrase `testpass`, DEK nota;
- `keys.x25519.age` + `identity.txt` — wrap con chiave age;
- `blob_zstd_gcm.bin` — un blob cifrato con DEK nota, plaintext noto;
- `blob_none.bin` — blob non cifrato;
- `vectors.json` — descrizione di ogni file: chiavi in chiaro, plaintext atteso, digest.

### File: `pkg/crypt/golden_test.go`
Legge `vectors.json` ed esegue: unwrap, open, confronto. **Se questo test fallisce dopo una modifica, il formato è cambiato: è un breaking change e va escalato a Opus, non "aggiustato" rigenerando i golden.**

### Fuzz
- `FuzzParseHeader`
- `FuzzOpen` (blob arbitrari con DEK fissa: non deve mai andare in panic né in loop)
- `FuzzUnwrapKeys`

### Definition of Done
- [ ] i golden file sono committati e il test li verifica
- [ ] i 3 fuzz girano 60 s ciascuno in CI senza crash
- [ ] copertura `pkg/crypt` ≥ 90 %

---

## Gate di fase 03

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/crypt/...` | ≥ 90 % |
| **GS-03.1** | bit-flip su 100 blob casuali | sempre `ErrIntegrity`, mai plaintext restituito |
| **GS-03.2** | chunk index alterato | sempre `ErrIntegrity` |
| **GS-03.3** | 100 000 nonce in modalità random | tutti distinti |
| **GS-03.4** | modalità convergente | blob identici per plaintext identici |
| **GS-03.5** | golden `vectors.json` | tutti verdi |
| **GS-03.6** | `go list -deps ./pkg/crypt \| wc -l` | dipendenze limitate a stdlib + age + x/term + x/sys + pkg/compress |
| **GS-03.7** | `grep -rn "fmt.*DEK\|Print.*passphrase" pkg/` | nessun risultato |
| G9 | `make deps-check` | `filippo.io/age`, `golang.org/x/term` documentati |
| G10 | `docs/security.md` | presente e completo |
| G11 | revisione Opus | **obbligatoria e non delegabile**: è codice crittografico |

**Deliverable documentali**: `docs/security.md` — modello di minaccia, cosa protegge e cosa no (i nomi dei layer, la dimensione dei blob e il numero di file restano visibili nel registry), gestione delle chiavi, recupero, avvertenza sulla env var, spiegazione del compromesso della modalità convergente, procedura di rotazione della passphrase (ri-wrap della sola DEK, senza ri-cifrare i dati).

**Rischi noti**
- `age` con scrypt impiega circa un secondo: in `list` e `info` ripetuti dà l'impressione di lentezza. Non ridurre il work factor: se serve, la fase 06 memorizza la `KeyMaterial` in memoria per la durata del processo.
- Limite di GCM: 2³² chunk con la stessa chiave. A 4 MiB per chunk sono 16 EiB, fuori portata. Documentarlo comunque in `docs/security.md`.
