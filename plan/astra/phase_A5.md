# Fase A5 — Fiducia nell'eseguibile, privilegi e profili operativi

**Obiettivo**: chi ripristina può ancorare il backup a un digest ottenuto fuori banda e rifiutare
prima di consegnare la passphrase; l'autoestraente non porta più il client Docker; gli esempi in
vetrina non sono più i più esposti.

**Rilievi coperti**: A08. **Decisione**: DA-04.

**Premessa da non tradire**: un programma dentro un'immagine **non può autenticare l'immagine che
lo contiene**. Nessuna parte di questa fase deve suggerire il contrario.

---

## A5.1 `--expect-digest` sul binario host

**Agente: Opus** (progetto) + **Sonnet** (implementazione)

### Il difetto, verificato

`pkg/restore/verify_stored.go:198,218,224` confronta digest dei chunk, del blob e dell'oggetto OCI —
tutti letti **dalla stessa sorgente modificabile**. Un digest ottenuto da chi può sostituire
l'immagine non prova la provenienza. Non esiste oggi alcun modo di ancorare la lettura a un valore
esterno: nessun flag di `restore`, `verify`, `tar` o `ls` lo accetta (blocco flag di
`newRestoreCommand`, `internal/cli/restore.go:193-208`).

Cosa esiste, e dove: `.github/workflows/release.yml:83-90` installa Cosign e firma con
`cosign sign-blob` gli **archivi di release**. Le immagini di backup generate dagli utenti non sono
firmate da nulla. È esattamente il limite che A08 descrive.

### Intervento

- Flag `--expect-digest sha256:…` su `restore`, `verify`, `tar`, `ls` del **binario host**.
- Semantica **per sorgente**, perché "risolvere il riferimento" non significa la stessa cosa
  ovunque:
  - *registry*: il punto di aggancio è `pkg/restore/source.go:92`, `remote.Get(ref, ropts...)`,
    che restituisce il descrittore. Confrontare `desc.Digest` con il valore atteso **prima** di
    scaricare i layer.
  - *oci-layout, tar, daemon*: non c'è un registry da interrogare. Si calcola il digest del
    manifest dell'immagine locale e si confronta. Va detto nella documentazione che qui l'ancora
    vale solo se il file arriva da un canale diverso da quello che ha fornito il digest.
- **Se non coincide: errore di classe integrità e uscita prima di leggere la passphrase o il
  keyfile.** L'ordine conta e va coperto da un test che verifica che il segreto non sia stato
  letto: non basta che il comando finisca in errore.
- Se il flag è assente, il comportamento è quello di oggi. Nessuna rottura.
- La firma della funzione deve rendere **impossibile** passarle il digest calcolato localmente
  sull'oggetto già scelto: prende il valore atteso dall'esterno e il descrittore dalla sorgente,
  mai un solo argomento da cui derivare entrambi.
- **Il flag non va aggiunto all'autoestraente.** Dentro l'immagine non c'è nulla da confrontare che
  l'autore dell'immagine non controlli: sarebbe teatro. Per quel percorso l'ancora è l'host che
  esegue `docker run …@sha256:…`.

## A5.2 Il client Docker esce dall'autoestraente

**Agente: Sonnet**

### Il difetto, verificato

`cmd/backimage-selfextract/commands.go:188` definisce `--remove-local-image`, `:292-294` lo esegue
tramite `dockerd.RemoveLocalImage` (`:25`). L'opzione richiede il socket del daemon dentro
l'ambiente di estrazione. Su un daemon rootful, l'accesso a quel socket consente operazioni
sull'host molto oltre la cancellazione di un'immagine, e il processo non è vincolato alla funzione
che l'utente intendeva invocare.

Nessun e2e esercita questo flag: l'unica copertura è unitaria, con la funzione sostituita
(`cmd/backimage-selfextract/commands_test.go:303-311`).

### Intervento

- Rimuovere il flag dall'autoestraente. Invocarlo produce un usage error che indica l'equivalente
  lato host, `backimage restore --remove-local-image`, che resta e gira dove il socket già c'è
  (dichiarato in `internal/cli/restore.go:204`, usato alla riga 318).
- Verificare con `scripts/check-deps.sh` che il pacchetto `dockerd` non sia più nel grafo delle
  dipendenze di `cmd/backimage-selfextract`: lo script già vieta cobra, go-containerregistry,
  quic-go e protobuf (`:32`), e va esteso.
- Nell'help e nella documentazione: il cleanup è un'operazione dell'host, non dell'estrattore.

## A5.3 Il profilo confinato diventa l'esempio primario

**Agente: Haiku**

### Lo stato attuale, verificato

`README.md:12` e `README.it.md:12` — il **primo** esempio di entrambi i readme — è
`docker run --rm --privileged`. Altri: `README.md:124,143`, `README.it.md:125,144`,
`docs/handbook.it.md:683-686`. `docs/FIDELITY.md:84` è invece già corretto nel descrivere cosa
`--privileged` compra.

Gli e2e, al contrario, sono già confinati: nessuno degli 11 script usa `--privileged` o monta
`docker.sock`, e `test/e2e/phase_06.sh:89,94` esegue l'autoestraente con
`--user "$(id -u):$(id -g)"`, passphrase da env e un solo volume. **La documentazione è più
esposta del comportamento che testiamo.**

### Intervento

Esempio primario, in tutti i readme e nell'handbook:

```
docker run --rm \
  --network none \
  --read-only --tmpfs /tmp \
  --cap-drop ALL --security-opt no-new-privileges \
  --user "$(id -u):$(id -g)" \
  -v "$PWD/restore:/restore" \
  ghcr.io/me/dumps@sha256:… \
  extract --out /restore --no-preserve-owner
```

Il profilo a fedeltà completa con `--privileged` resta, più in basso, con: cosa compra
(ownership, device, ACL, xattr), cosa costa, e la raccomandazione di confinarlo in VM quando serve
davvero. Nessuna pagina deve promettere insieme "ripristino illimitato di device e capability" e
"assenza dei privilegi necessari".

Aggiungere la procedura di ancoraggio esterno del digest: come ottenerlo fuori banda, come passarlo
a `--expect-digest`, e perché il digest letto dalla stessa sorgente non serve a niente.

## A5.4 e2e dell'autoestraente in profilo confinato

**Agente: Haiku**

`test/e2e/` non ha alcuno script che esercita l'autoestraente con entrypoint sostituito o con il
profilo confinato completo. È l'accettazione richiesta da A08.

- `test/e2e/phase_A5.sh`: costruisce un'immagine di backup, ne **sostituisce l'entrypoint**, e
  verifica che il flusso con `--expect-digest` rifiuti **prima** di ricevere il segreto.
- Restore di file ordinari senza privilegi, in profilo confinato completo.
- Gate separato, esplicito, per ownership, xattr e device: la fedeltà completa è un profilo
  dichiarato, non il default.
- Non montare automaticamente il socket Docker per comodità in nessuno script.

---

## Cosa questa fase non fa

- **Non firma le immagini di backup.** `backup --sign` e `restore --verify-signature` con cosign
  restano una feature da decidere a parte: richiedono gestione chiavi, referrers OCI, un progetto
  per il caso offline (`oci-layout`, `tar`, `daemon`) e collidono con il vincolo di dipendenze di
  `scripts/check-deps.sh:32` se la verifica deve stare anche nell'autoestraente.
- **Non rende autenticabile l'autoestraente dall'interno.** Resta un limite di modello, dichiarato
  nella documentazione.

**Documentazione**: `README.md`, `README.it.md`, `docs/handbook.it.md`, `docs/selfextract.md`,
`docs/FIDELITY.md`, `docs/security.md`, `docs/restore.md`, `CHANGELOG.md`.

---

## Uscita di fase

- Chi ha un digest fidato può rifiutare un'immagine sostituita prima di consegnare la passphrase.
- L'autoestraente non contiene un client Docker né chiede il socket del daemon.
- Gli esempi consigliati coincidono con i profili che i test esercitano.
