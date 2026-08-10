# Fase 08 — Trasporto TCP/TLS, protocollo, `listen-remote`

**Obiettivo**: risolvere il caso posto dall'utente — *sulla macchina da salvare non c'è spazio disco per l'immagine*. Il client fa streaming verso un server che assembla e pusha, **senza mai ricevere credenziali permanenti** (D11) e **senza vedere il plaintext** (D12).

**Riferimento decisioni**: D11, D12, D13.

> Nota di progetto: il server non deve limitarsi a spostare il problema. Deve **caricare i blob sul registry man mano che arrivano**, senza accumulare l'immagine su disco.

---

## 08.1 Schema protobuf dei messaggi di controllo

**Agente: Sonnet**

### File: `pkg/protocol/backimage.proto`, `pkg/protocol/*.pb.go` (generati)

```protobuf
syntax = "proto3";
package backimage.v1;
option go_package = "github.com/manprint/backimage/pkg/protocol";

message Hello {
  string client_version = 1;
  uint32 protocol_version = 2;      // 1
  string session_id = 3;            // client-generated, used for resume
}

message HelloAck {
  string server_version = 1;
  uint32 protocol_version = 2;
  uint64 max_bytes = 3;             // 0 = no quota
  repeated string allowed_repo_prefixes = 4;
  bool resumable = 5;
  repeated string known_blob_digests = 6;  // for resume
}

message BackupStart {
  string reference = 1;             // ghcr.io/me/dumps:daily
  bytes  manifest_json = 2;         // index.Manifest, already final except layer digests
  uint32 layer_count = 3;
  uint64 estimated_bytes = 4;
  repeated Platform platforms = 5;
  bytes  selfextract_amd64 = 6;     // sent by the client: the server never builds binaries
  bytes  selfextract_arm64 = 7;
  bytes  metadata_layer = 8;        // manifest/chunks/index/keys layer tar, built client-side
}

message Platform { string os = 1; string architecture = 2; }

message LayerStart { uint32 index = 1; uint64 size = 2; string sha256 = 3; }
message LayerEnd   { uint32 index = 1; string digest = 2; }

message Token {
  string value = 1;
  int64  expires_at_unix = 2;
  string repository = 3;
  repeated string actions = 4;
}

message TokenRequest { string repository = 1; repeated string actions = 2; }

message Progress { uint32 layer = 1; uint64 uploaded = 2; bool skipped = 3; }

message BackupEnd { string digest = 1; uint64 bytes_uploaded = 2; uint32 blobs_skipped = 3; }

message Error { uint32 kind = 1; string message = 2; string hint = 3; }

message ClientMessage {
  oneof msg {
    Hello hello = 1;
    BackupStart backup_start = 2;
    LayerStart layer_start = 3;
    LayerEnd layer_end = 4;
    Token token = 5;
    Error error = 6;
    Cancel cancel = 7;
  }
}

message ServerMessage {
  oneof msg {
    HelloAck hello_ack = 1;
    TokenRequest token_request = 2;
    Progress progress = 3;
    BackupEnd backup_end = 4;
    Error error = 5;
    LayerAck layer_ack = 6;
  }
}

message LayerAck { uint32 index = 1; bool skipped = 2; }
message Cancel { string reason = 1; }
```

### Prescrizioni
- I dati dei layer **non** passano da protobuf: viaggiano come frame `Data` grezzi (08.2). Protobuf serve solo al controllo.
- `protocol_version` incompatibile → `Error` e chiusura, con messaggio che riporta entrambe le versioni.
- Il `.pb.go` generato va **committato** e `make check` deve verificare che rigenerarlo non produca differenze (target `make proto-check`); così l'implementatore non ha bisogno di `protoc` per compilare.

---

## 08.2 Framing

**Agente: Sonnet**

### File: `pkg/protocol/frame.go`

```
+--------+--------+------------------+
| type   | length | payload          |
| 1 byte | 4 be   | length bytes     |
+--------+--------+------------------+
```

```go
// FrameType identifies the payload of a frame.
type FrameType uint8

const (
	FrameControl FrameType = 1 // protobuf ClientMessage/ServerMessage
	FrameData    FrameType = 2 // raw layer bytes
	FrameKeepalive FrameType = 3
)

// MaxFrameSize is the largest accepted payload.
const MaxFrameSize = 4 << 20

// WriteFrame writes one frame to w.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error

// ReadFrame reads one frame into buf, growing it when needed.
func ReadFrame(r io.Reader, buf []byte) (FrameType, []byte, error)
```

### Prescrizioni
- `length > MaxFrameSize` → errore immediato, connessione chiusa. **Difesa contro l'esaurimento di memoria: obbligatoria.**
- I frame `FrameData` appartengono al layer dichiarato dall'ultimo `LayerStart`: nessun identificatore per frame, si risparmia banda ed è inequivocabile perché il flusso è sequenziale.
- Keepalive ogni 30 s di inattività; timeout di lettura a 120 s.
- Nessuna allocazione per frame in stato stazionario: buffer riusato.

### Test
- round-trip di frame di 0, 1, 4 MiB byte;
- frame oversize → errore, nessuna allocazione grande (verificare con `testing.AllocsPerRun` e un contatore);
- lettore che consegna 1 byte alla volta → frame corretti;
- fuzz `FuzzReadFrame`.

---

## 08.3 `Dialer` e `Listener`, TCP+TLS

**Agente: Sonnet**

### File: `pkg/transport/transport.go`, `pkg/transport/tcp.go`

```go
// Stream is a bidirectional byte stream with deadlines.
type Stream interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
}

// Dialer opens a Stream to a server.
type Dialer interface {
	Dial(ctx context.Context, addr string) (Stream, error)
	Name() string // "tcp" or "quic"
}

// Listener accepts Streams.
type Listener interface {
	Accept(ctx context.Context) (Stream, error)
	Addr() net.Addr
	Close() error
}

// Config holds the TLS material shared by both transports.
type Config struct {
	TLS         *tls.Config
	Keepalive   time.Duration
	IdleTimeout time.Duration
}

// NewDialer returns the dialer registered under name.
func NewDialer(name string, cfg Config) (Dialer, error)

// NewListener returns the listener registered under name.
func NewListener(name, addr string, cfg Config) (Listener, error)
```

### Prescrizioni (sicurezza)
- **TLS 1.3 obbligatorio** (`MinVersion: tls.VersionTLS13`). Nessun modo di disattivarlo.
- Autenticazione del client: **mTLS** (`ClientAuth: RequireAndVerifyClientCert`) **oppure** token pre-condiviso nell'`Hello` (`--auth-token`, confrontato con `subtle.ConstantTimeCompare`). Almeno uno dei due è obbligatorio: un server senza autenticazione **non si avvia**, salvo `--insecure-no-auth` che stampa un avviso vistoso.
- Il server può generare un certificato autofirmato all'avvio (`--tls-self-signed`) e stamparne l'impronta SHA-256; il client la accetta con `--tls-pin <sha256>`. Questo evita di richiedere una PKI per un uso in LAN, senza rinunciare all'autenticazione del server.
- `--tls-cert`/`--tls-key`/`--tls-ca` per il caso con PKI.

### Test
- handshake TLS 1.3 riuscito, TLS 1.2 rifiutato;
- pinning: impronta giusta → ok, sbagliata → errore;
- mTLS: senza certificato client → rifiuto;
- server senza auth e senza `--insecure-no-auth` → non parte;
- deadline rispettate.

---

## 08.4 Macchina a stati della sessione lato server

**Agente: Sonnet**

### File: `pkg/server/session.go`

```
       ┌──────┐  Hello    ┌───────────┐ BackupStart ┌─────────┐
init → │ new  │ ────────→ │ greeted   │ ──────────→ │ ready   │
       └──────┘           └───────────┘             └─────────┘
                                                         │ LayerStart
                                                         ▼
                                                   ┌───────────┐
                                    Data frames →  │ receiving │ ── LayerEnd ─┐
                                                   └───────────┘              │
                                                         ▲                    │
                                                         └────────────────────┘
                                                     (layer successivo)
                                        tutti i layer ricevuti → BackupEnd → closed
```

Ogni transizione non prevista → `Error{kind: usage}` e chiusura. Nessuno stato implicito, nessuna tolleranza.

### Prescrizioni
- **Nessuna scrittura su disco per i dati**: ogni layer ricevuto viene caricato sul registry in streaming. Poiché il registry richiede il digest alla fine dell'upload, e il client lo ha già calcolato e dichiarato in `LayerStart.sha256`, il server può usare l'upload monolitico/a pezzi conoscendo il digest fin dall'inizio: `POST /uploads/` → `PATCH` a pezzi da 32 MiB man mano che arrivano i frame → `PUT ?digest=<sha256 dichiarato>`. Il registry verifica il digest: se il client mente, l'upload viene rifiutato dal registry stesso. **Il server verifica comunque** il digest mentre passa i byte, e interrompe subito in caso di divergenza.
- Un buffer di al massimo 64 MiB per sessione. Quota di memoria totale = `--max-sessions × 64 MiB`.
- `--work-dir` esiste solo per un eventuale fallback (`--spool` esplicito): per default il server **non** scrive nulla su disco. Va documentato come proprietà, non come dettaglio.
- Prima di ricevere un layer, il server fa `HEAD /blobs/<digest>`: se esiste, risponde `LayerAck{skipped:true}` e il client **non invia i dati**. Dedup gratuita già in questa fase.

### Test
- transizioni valide e non valide (tabella con 12 casi);
- digest divergente → errore entro 64 MiB dall'inizio della divergenza;
- `LayerAck{skipped}` → il client salta l'invio (verificato con un contatore di byte);
- picco di memoria per sessione < 96 MiB su un layer da 1 GiB.

---

## 08.5 Flusso `TokenRefresh`

**Agente: Sonnet** — *il punto che rende praticabili i backup da 50 GB in modalità remota*

### Meccanica prescritta

1. In `BackupStart` il client **non** invia token.
2. Il server, quando gli serve, invia `TokenRequest{repository, actions}`.
3. Il client conia il token con il proprio `registry.Provider` (05.3) e risponde `Token{value, expires_at}`.
4. Il client invia **proattivamente** un `Token` aggiornato quando manca il 40 % della vita di quello corrente, senza attendere richieste.
5. Il server usa un `registry.NewStaticProvider` la cui `Get` attende, se necessario, l'arrivo di un token valido, con timeout di 30 s → in caso di scadenza, `Error{kind: network}` e sessione chiusa.
6. Il token non viene mai scritto su disco né loggato.

### Test
- token con vita 5 s, backup che dura 30 s → completa, con almeno 5 rinnovi;
- client che smette di rispondere alle `TokenRequest` → il server chiude con errore entro 30 s, senza restare appeso;
- il token non compare nei log del server (grep sull'output catturato).

---

## 08.6 Quote, ACL, limiti

**Agente: Sonnet**

### Flag di `listen-remote`

| Flag | Default | Significato |
|---|---|---|
| `--bind-address` | `0.0.0.0:7575` | indirizzo di ascolto |
| `--udp` | false | usa QUIC (fase 09) |
| `--tls-cert` / `--tls-key` / `--tls-ca` | | PKI |
| `--tls-self-signed` | false | certificato effimero + stampa dell'impronta |
| `--auth-token` / `--auth-token-file` | | token pre-condiviso |
| `--insecure-no-auth` | false | **sconsigliato**, avviso vistoso |
| `--allow-repo` | vuoto (= tutto) | prefissi di repository consentiti, ripetibile |
| `--max-sessions` | 4 | sessioni concorrenti |
| `--max-bytes` | 0 (illimitato) | byte massimi per sessione |
| `--rate-limit` | 0 | byte/s per sessione |
| `--metrics-address` | vuoto | `/healthz` e `/metrics` Prometheus |
| `--log-format` | text | `text\|json` |

### Prescrizioni
- `--allow-repo ghcr.io/me/` → una `BackupStart` verso `ghcr.io/altro/x` viene rifiutata **prima** di ricevere byte.
- Superamento di `--max-bytes` → `Error` e chiusura, con il conteggio nel messaggio.
- `--max-sessions` raggiunto → nuove connessioni rifiutate con un errore chiaro, non lasciate in attesa indefinita.
- Metriche minime: sessioni attive, byte ricevuti, byte caricati, layer saltati, errori per tipo, durata delle sessioni.

### Test
- repo non consentito → rifiuto immediato, zero byte ricevuti;
- quota superata → chiusura con messaggio;
- quinta sessione con `--max-sessions 4` → rifiuto immediato;
- `/healthz` risponde 200; `/metrics` espone i contatori attesi.

---

## 08.7 Client `--remote`

**Agente: Sonnet**

### Sinossi

```
backimage backup ./myfiles --repo ghcr.io/me/dumps --remote 10.10.2.20:7575 [--udp] [flags]
```

### Prescrizioni
- La pipeline lato client è **identica** a quella locale (05.5) fino alla produzione del layer, poi invece di caricare su registry invia i frame. Riusare lo stesso codice: la differenza è l'implementazione dell'`Uploader`.
  ```go
  // Uploader publishes one finished layer.
  type Uploader interface {
      Upload(ctx context.Context, index int, size int64, sha string, r io.Reader) (skipped bool, err error)
  }
  ```
  Due implementazioni: `registry.DirectUploader` (fase 05) e `remote.StreamUploader` (questa fase). Il resto della pipeline non sa quale sta usando. **Se serve duplicare la pipeline, l'astrazione è sbagliata: fermarsi e segnalare.**
- Il client **non** ha bisogno di spazio per il layer intero: con l'uploader remoto, il layer viene trasmesso mentre viene prodotto. Il vincolo di spazio temporaneo di 05.5 **decade** in modalità remota, ed è il motivo per cui questa modalità esiste. Va verificato con un test che esegue il backup in un container con `--tmpfs /tmp:size=64m`.
  - Conseguenza tecnica: senza file temporaneo non si può ricalcolare il layer per un retry. Se la connessione cade a metà layer, quel layer **riparte da capo** dalla sorgente (rileggendo i file). Comportamento accettabile e da documentare; il checkpoint (08.8) lavora a granularità di layer.
- Cifratura e compressione restano lato client (D12). `--server-side-compress` è opt-in e stampa un avviso: *"il server vedrà i tuoi dati in chiaro"*.

### Test
- backup remoto verso un server in-process → immagine sul registry identica a quella prodotta in locale (confronto dei digest);
- backup con `/tmp` da 64 MiB e layer da 512 MiB → riesce;
- `--server-side-compress` → avviso presente.

---

## 08.8 Ripresa dopo caduta della connessione

**Agente: Sonnet**

### Prescrizioni
- `session_id` deterministico, come l'ID di checkpoint di 05.4.
- Alla riconnessione, il client rimanda `Hello` con lo stesso `session_id`; il server risponde con `known_blob_digests`, cioè i layer già caricati sul registry per quel riferimento.
- Il client salta i layer già presenti. Poiché i layer sono deterministici (04.3) e la pipeline è deterministica (01.4), gli stessi input producono gli stessi digest: la ripresa è corretta.
- Ritentativi automatici di connessione: 5, con backoff 1/2/4/8/16 s. Poi errore.

### Test
- uccidere la connessione dopo 2 layer, riprendere → i primi 2 non vengono ritrasmessi;
- server riavviato nel frattempo → la ripresa funziona comunque (lo stato sta sul registry, non nel server: proprietà da testare esplicitamente);
- 5 tentativi falliti → errore con exit 6.

---

## 08.9 End-to-end a due processi

**Agente: Sonnet**

### File: `test/e2e/phase_08.sh`

```
1.  avvia registry:2
2.  avvia:  backimage listen-remote --bind-address 127.0.0.1:7575 \
              --tls-self-signed --auth-token-file tok --allow-repo localhost:5000/
    cattura l'impronta del certificato
3.  backimage backup ./tree --repo localhost:5000/e2e/rem:t1 \
      --remote 127.0.0.1:7575 --tls-pin <impronta> --auth-token-file tok \
      --passphrase-file pass.txt
4.  docker pull + docker run … tar > out.tar   → confronto: ZERO differenze
5.  confronto del digest con il backup fatto in locale sugli stessi dati → IDENTICO
6.  fault injection: uccidi il server a metà, riavvialo, rilancia il client → completa
7.  prova con --allow-repo che non copre il target → rifiuto immediato
8.  prova senza --auth-token → rifiuto
9.  verifica che sul server non ci siano file residui in /tmp e in --work-dir
```

### Definition of Done
- [ ] `make e2e PHASE=08` esce 0
- [ ] il passo 5 dimostra che remoto e locale producono la stessa immagine
- [ ] il passo 9 dimostra che il server non usa disco

---

## Gate di fase 08

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/transport/... ./pkg/protocol/... ./pkg/server/...` | ≥ 85 % |
| G8 | `make e2e PHASE=08` | exit 0 |
| **GS-08.1** | digest immagine locale vs remota | identici |
| **GS-08.2** | client con `/tmp` da 64 MiB, layer da 512 MiB | riesce |
| **GS-08.3** | server: file su disco durante e dopo il backup | zero |
| **GS-08.4** | token da 5 s su backup da 30 s | completa, ≥ 5 rinnovi |
| **GS-08.5** | frame oversize | connessione chiusa, memoria non allocata |
| **GS-08.6** | TLS 1.2 | rifiutato |
| **GS-08.7** | server senza auth | non si avvia |
| **GS-08.8** | ripresa dopo riavvio del server | i layer già caricati non si ritrasmettono |
| **GS-08.9** | `make proto-check` | il `.pb.go` rigenerato coincide con quello committato |
| **GS-08.10** | grep di token/passphrase nei log del server | nessun risultato |
| G10 | `docs/remote.md`, `docs/protocol.md` | presenti |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**
- `docs/remote.md`: quando serve, come si mette in sicurezza (le tre modalità di autenticazione), quote e ACL, esempio completo con systemd unit, cosa vede e cosa non vede il server.
- `docs/protocol.md`: framing, messaggi, macchina a stati, flusso dei token, versionamento.

**Rischi noti**
- Il caso "il client mente sul digest dichiarato" è coperto due volte (server e registry): non rimuovere nessuna delle due verifiche per ottimizzare.
- Il riavvio di un layer da capo dopo una caduta rilegge i file sorgente: se i file sono cambiati nel frattempo, il layer avrà un digest diverso e la ripresa produrrà un'immagine incoerente con il manifest già inviato. **Mitigazione obbligatoria**: al riavvio di un layer, il client ricalcola e, se il digest differisce da quello dichiarato, annulla il backup con un errore esplicito ("i file sorgente sono cambiati durante il backup"), invece di produrre un'immagine sbagliata.
