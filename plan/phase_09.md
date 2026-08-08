# Fase 09 — Trasporto QUIC (`--udp`)

**Obiettivo**: implementare il trasporto QUIC dietro l'interfaccia già definita in 08.3, e — punto altrettanto importante — **misurare** se e quando serve davvero.

**Premessa onesta, da tenere presente durante tutta la fase**: su LAN il collo di bottiglia è la lettura del disco e la compressione, non il trasporto; QUIC in spazio utente consuma più CPU per byte del TCP del kernel. QUIC conviene su collegamenti con RTT alto e/o perdita di pacchetti. Questa fase deve produrre i numeri che confermano o smentiscono l'utilità del flag, e la documentazione deve dire la verità che emerge dai numeri.

**Riferimento decisioni**: D13.

---

## 09.1 Implementazione QUIC

**Agente: Sonnet**

### File: `pkg/transport/quic.go`

Implementare `Dialer` e `Listener` con `github.com/quic-go/quic-go`, registrandoli sotto il nome `"quic"`. Nessuna modifica alle interfacce: se serve cambiarle, l'astrazione di 08.3 era sbagliata — fermarsi e segnalare.

### Prescrizioni
- ALPN: `backimage/1`. Il server rifiuta gli altri.
- Una connessione QUIC, **uno stream** per sessione di backup. Il multi-stream non serve: il flusso è sequenziale e l'eliminazione del head-of-line blocking fra stream non porta vantaggi qui. Prevedere però `--streams N` (default 1) per l'esperimento di 09.3, con i layer distribuiti su N stream.
- Keepalive: `quic.Config.KeepAlivePeriod = 15s`; `MaxIdleTimeout = 120s`.
- Finestre: `InitialStreamReceiveWindow` e `InitialConnectionReceiveWindow` ampie (16 MiB / 32 MiB) e `MaxStreamReceiveWindow`/`MaxConnectionReceiveWindow` a 64 MiB / 128 MiB. Con RTT alto, finestre piccole sono la prima causa di throughput scarso: la configurazione predefinita di quic-go è conservativa.
- `DisablePathMTUDiscovery: false`.
- Il certificato e il pinning funzionano esattamente come in 08.3: stesso codice, stessa `Config`.

### Test
- gli stessi test di 08.3, eseguiti in tabella su entrambi i trasporti (`tcp`, `quic`): **un solo corpo di test parametrizzato**, non due copie;
- ALPN errato → rifiuto;
- MTU piccola (1200) → funziona comunque.

---

## 09.2 Flag `--udp` su client e server

**Agente: Haiku**

- `listen-remote --udp` → `NewListener("quic", …)`; senza → `"tcp"`.
- `backup --remote host:port --udp` → `NewDialer("quic", …)`.
- Se il server è in QUIC e il client in TCP (o viceversa), l'errore deve essere comprensibile: il client che tenta TCP verso una porta UDP ottiene "connection refused"; aggiungere un suggerimento nel messaggio: *"il server potrebbe essere in ascolto con --udp: riprova aggiungendo --udp"*.
- Nota d'uso da documentare: TCP e QUIC possono coesistere sulla **stessa** porta (una in TCP, una in UDP). Prevedere `listen-remote --udp --also-tcp` che apre entrambi, così il client sceglie.

### Test
- matrice client×server su {tcp, quic}: le due combinazioni corrette funzionano, le due incrociate danno un errore con il suggerimento;
- `--also-tcp`: entrambi i client funzionano.

---

## 09.3 Tuning

**Agente: Sonnet**

Parametri da esporre come flag **nascosti** (`--x-quic-*`, marcati `Hidden: true`), solo per la sperimentazione di 09.4:
`--x-quic-streams`, `--x-quic-window`, `--x-quic-gso` (abilita/disabilita `SetTxGSO` tramite variabile d'ambiente `QUIC_GO_DISABLE_GSO`), `--x-quic-cc` se quic-go espone la scelta del controllo di congestione.

### Prescrizioni
- I flag nascosti non compaiono in `--help` né in `docs/cli.md`, ma sono documentati in `docs/transport-benchmark.md`.
- Nessuno di questi flag deve essere necessario per l'uso normale: se il default richiede tuning, il default è sbagliato.

---

## 09.4 Harness di benchmark

**Agente: Sonnet**

### File: `test/bench/transport/main.go` e `test/bench/transport/run.sh`

Un binario che trasferisce N GiB di dati sintetici da un client a un server in-process **usando il vero stack** (frame, TLS, sessione), misurando throughput e CPU.

### Matrice obbligatoria

| Dimensione | RTT | Perdita | Trasporti |
|---|---|---|---|
| 4 GiB | 0,1 ms (loopback) | 0 % | tcp, quic |
| 4 GiB | 10 ms | 0 % | tcp, quic |
| 4 GiB | 10 ms | 0,1 % | tcp, quic |
| 4 GiB | 50 ms | 0,5 % | tcp, quic |
| 4 GiB | 100 ms | 1 % | tcp, quic |
| 4 GiB | 200 ms | 2 % | tcp, quic |

RTT e perdita si impongono con `tc qdisc add dev lo root netem delay Xms loss Y%` (richiede root; lo script deve rilevarlo e, se manca, saltare con un messaggio esplicito invece di produrre numeri falsi).

Per ogni cella misurare: throughput (MiB/s), tempo totale, CPU del client, CPU del server, ritrasmissioni (`ss -ti` per TCP, statistiche di quic-go per QUIC).

Ogni cella va ripetuta 3 volte; si riporta la mediana.

### Prescrizioni
- Il benchmark **non** deve essere parte di `make check` (troppo lungo e richiede root): target dedicato `make bench-transport`.
- I dati sintetici vanno generati una volta e riusati, per non misurare il generatore.

---

## 09.5 Documento dei risultati

**Agente: Haiku** (redazione), **Opus** (conclusioni)

### File: `docs/transport-benchmark.md`

Deve contenere:
1. metodologia (hardware, kernel, versioni, comandi esatti);
2. la tabella completa dei risultati;
3. un grafico testuale o una tabella di sintesi "guadagno QUIC vs TCP" per cella;
4. **una raccomandazione operativa esplicita**, del tipo:
   > Usa `--udp` quando l'RTT supera *X* ms o la perdita supera *Y* %. Sotto queste soglie, TCP è più veloce e consuma meno CPU.
   I valori di *X* e *Y* si ricavano dai numeri, non si scelgono a priori.
5. una sezione "se i numeri non giustificano QUIC": in tal caso il flag resta, documentato come utile solo su collegamenti degradati, e `docs/remote.md` lo dice chiaramente. **Non si truccano i numeri e non si rimuove il flag**: l'utente lo ha chiesto e la risposta onesta è comunque un risultato.

### Definition of Done
- [ ] la tabella contiene numeri misurati su tutte le celle eseguibili
- [ ] la raccomandazione cita soglie numeriche derivate dai dati
- [ ] `docs/remote.md` rimanda a questo documento

---

## Gate di fase 09

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/transport/...` | ≥ 85 % |
| G8 | `make e2e PHASE=09` | exit 0 (ripete la e2e di fase 08 con `--udp`) |
| **GS-09.1** | i test di trasporto parametrizzati | verdi su `tcp` **e** `quic` |
| **GS-09.2** | backup remoto con `--udp` | immagine identica a quella prodotta via TCP |
| **GS-09.3** | client/server con trasporti incrociati | errore con suggerimento `--udp` |
| **GS-09.4** | `make bench-transport` | completa e produce la tabella |
| **GS-09.5** | assenza di duplicazione | `pkg/server` e `pkg/protocol` invariati rispetto alla fase 08 (`git diff --stat` limitato a `pkg/transport` e ai flag) |
| G10 | `docs/transport-benchmark.md` | presente, con numeri reali e raccomandazione |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**: `docs/transport-benchmark.md`; aggiornamento della sezione trasporto in `docs/remote.md` e in `README.md`.

**Rischi noti**
- Molte reti aziendali e cloud bloccano o limitano UDP: `--udp` può risultare più lento o non funzionare affatto. Il client deve rilevare il fallimento dell'handshake QUIC entro 10 s e suggerire di riprovare senza `--udp`.
- `netem` su `lo` influenza anche il traffico verso il registry se è locale: nel benchmark, il registry non deve essere coinvolto (il benchmark misura solo il trasporto, con un server che scarta i byte).
- Se `GS-09.5` mostra modifiche a `pkg/server` o `pkg/protocol`, l'astrazione della fase 08 non reggeva: è un segnale da portare a Opus, non da assorbire in silenzio.
