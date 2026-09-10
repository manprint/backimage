# Fase A1 — Autenticità del percorso cifrato e perdita dati — **P0**

**Obiettivo**: un backup che si dichiara cifrato non può contenere blob non autenticati, e nessun
restore filtrato può cancellare o estrarre più di quanto l'utente ha chiesto.

**Rilievi coperti**: A01 (P0), A12, A13.

**Precondizione**: fase A0 chiusa, altrimenti i test locali possono misurare un estrattore
incorporato diverso dal codice che si sta correggendo.

---

## A1.1 Separazione degli opener: cifrato e non cifrato sono percorsi distinti

**Agente: Opus** (progetto) + **Sonnet** (implementazione)

### Il difetto, verificato

`pkg/crypt/chunk.go:191-193`, in `opener.Open`:

```go
switch h.AEAD {
case aeadNone:
	// Clear blob; keyed or keyless opener both may read it.
	return append(dst, blob[n:]...), h.Codec, nil
```

L'opener restituisce il payload in chiaro **anche quando possiede la chiave**. Da qui:

- `pkg/index/model.go:389` — `ReadIndex` accetta zstd grezzo quando `!crypt.IsEnvelope(raw)`, e in
  quel caso costruisce da sé un opener senza chiave.
- `pkg/index/private.go:149` — `ReadPrivate` pretende il magic dell'envelope, che però non
  garantisce AES-GCM: un envelope con `AEAD=none` passa.
- `pkg/recovery/recovery.go:358-380` — `PlainChunk` usa lo stesso opener permissivo per i dati.

Esito riprodotto dalla review: `encrypted=true full_verify=true forged_tar_accepted=true`.

### Compatibilità: nessun rischio, verificato

Il sealer nasce solo con la chiave (`pkg/backup/pipeline.go:894-900`, `pkg/server/stream.go:304`;
guard presente dal commit `157566b`), e `crypt.NewSealer(nil)` ha come unici chiamanti i test
(`pkg/crypt/chunk_test.go:489`). **Nessuna release ha mai prodotto blob `aeadNone` dentro un backup
cifrato.** La compatibilità va mantenuta soltanto per i backup dichiarati non cifrati.

I punti di costruzione da toccare sono **cinque**, tutti fuori dai test:
`pkg/crypt/chunk.go` (le due factory), `pkg/index/model.go:391` (`NewOpener(nil)`),
`pkg/recovery/recovery.go:151` (`NewOpener(nil)`) e `:270` (`NewOpener(km)`) per gli opener;
`pkg/backup/pipeline.go:899` e `pkg/server/stream.go:313` per i sealer.

### Intervento

- `pkg/crypt`: due costruttori distinti e un tipo distinto per ciascuno.
  `NewKeyedOpener(km)` **rifiuta** `aeadNone` con `ErrIntegrity`; `NewClearOpener()` accetta solo
  `aeadNone` e rifiuta `aeadAES256GCM` con l'errore odierno "key material required".
  `NewOpener` resta come alias deprecato solo se serve a non spargere il cambiamento; meglio
  rimuoverlo e aggiornare i cinque punti elencati sopra.
  Nota di propagazione: cambiare la firma di `ReadIndex`/`ReadPrivate` tocca anche i costruttori di
  fixture nei test (`internal/cli/restore_test.go:134`,
  `cmd/backimage-selfextract/commands_test.go:105`, `pkg/index/model_test.go`,
  `pkg/index/private_test.go`). Sono aggiornamenti meccanici, ma vanno previsti nella stima.
- La scelta del costruttore dipende da `manifest.Encryption.Enabled`, **mai** dall'header del blob.
  `pkg/recovery/recovery.go:150` (in `newBackup`) e `:270` (in `Unlock`) sono i due punti dove la
  decisione va presa una volta.
- `index.ReadIndex` e `index.ReadPrivate` ricevono l'aspettativa in modo esplicito (parametro o
  opener tipizzato) e non sniffano più `IsEnvelope` per decidere la politica.
- Validare, prima di consegnare qualunque byte: schema, versione, ruolo, tipo di cifratura atteso e
  **presenza dei blob obbligatori** (in schema 2 il blob private non è opzionale).

### A1.2 Policy `require-encryption` del chiamante

**Agente: Sonnet**

L'opener stretto impedisce il downgrade *dentro* un backup cifrato, non la sostituzione dell'intero
backup con uno schema 1 in chiaro. Serve una politica del chiamante: quando l'utente fornisce una
passphrase o un keyfile, un backup non cifrato è un errore, non un successo silenzioso.

Punti di aggancio: `internal/cli/restore.go:49` `addSourceFlags`, dove i flag del segreto vengono
dichiarati per tutti i comandi di lettura, e `cmd/backimage-selfextract/commands.go:96`
`openBackup`, che è il punto unico dell'autoestraente. La politica va applicata in entrambi,
altrimenti vale su un eseguibile e non sull'altro.

- Errore di classe integrità/formato, non di autenticazione, e messaggio che nomina la sostituzione
  come ipotesi.
- Flag di uscita esplicito per chi legge deliberatamente backup misti in automazione.

**Cambio di comportamento da documentare**: passphrase più backup in chiaro passa da "ignora la
passphrase e riesce" a "errore".

## A1.3 `--overwrite` non cancella figli estranei

**Agente: Sonnet**. **Rilievo**: A12.

### Il difetto, verificato

`pkg/archive/extract_unix.go:323-330`, in `createOne` (il `RemoveAll` è alla riga 327):

```go
if _, err := os.Lstat(target); err == nil {
	if !x.opts.Overwrite { ... }
	if err := os.RemoveAll(target); err != nil { ... }
}
```

Quando l'entry è una directory e sulla destinazione esiste già una directory, `RemoveAll` cancella
**tutti i suoi figli**, compresi quelli che il backup non contiene.

### Intervento

- `RemoveAll` solo quando il **tipo** dell'oggetto esistente differisce da quello dell'entry.
- Directory su directory: nessuna cancellazione, si prosegue e i metadati vengono finalizzati come
  già avviene per le altre directory. È la semantica di `tar -x`.
- File su file: troncamento come oggi.

**Cambio di comportamento da documentare**: `--overwrite` passa da "sostituisci l'albero" a
"sovrapponi l'albero".

## A1.4 `--continue` non annulla i filtri

**Agente: Sonnet**. **Rilievo**: A13.

### Il difetto, verificato

`internal/cli/restore.go:277-285`: con `--continue` lo stream diventa `StreamTarPartial`
(riga 279), che ignora `selected`. Poi `restoreExtract(cmd, stream, selected != nil, …)`
(firma alla riga 371) riceve `alreadyFiltered=true` e alla riga 401 azzera includes ed excludes.
Risultato:
`--continue --include '**/*.pdf'` **estrae tutto il backup**, e con `--overwrite` lo scrive sopra la
destinazione.

### Intervento, in due passi

1. **Immediato e sicuro**: `alreadyFiltered` diventa `selected != nil && !keepGoing`, così
   l'extractor riapplica i filtri. Per l'uscita tar (`restoreTar`), dove non c'è extractor a valle,
   la combinazione `--continue` con `--include/--exclude` viene **rifiutata** con usage error
   finché non esiste il punto 2.
2. **Completo**: `StreamSelectedTarPartial`, che unisce selezione per entry e tolleranza ai chunk
   danneggiati, mantenendo un range per entry come fa oggi `StreamTarPartial` (unire i range
   farebbe perdere i vicini di un chunk rotto).

**Accettazione della fase**

Dalla review, per A01: trasformare la prova in test di **rifiuto**; coprire il downgrade di data,
private e index singolarmente e insieme; tutte le sorgenti (`LocalSource`, OCI/registry, oci-layout,
daemon); **entrambi** gli eseguibili; i comandi `verify`, `tar`, restore completo, selettivo e
`--continue`; con e senza `--no-verify`. L'errore deve precedere il rilascio di qualunque byte di
plaintext e classificarsi come integrità/formato, senza alcun segnale di successo.

Per A12: destinazione con figli estranei, tipi discordanti, nomi ripetuti nel tar.
Per A13: `--continue` con `--include`, con `--exclude`, con `--strip-components`, verso tar e verso
filesystem.

**Test interni**: `pkg/crypt`, `pkg/index`, `pkg/recovery`, `pkg/archive`, `internal/cli`.
`pkg/index` è al 69,4% di copertura, la più bassa del progetto, ed è dove vive metà di A01: la fase
non è chiusa se la copertura dei percorsi di rifiuto non sale.

**e2e**: nuovo `test/e2e/phase_A1.sh` — costruisce un backup cifrato, ne falsifica data, private e
index in un layout locale, e verifica il rifiuto su entrambi gli eseguibili e su tutte le forme di
lettura. Più i due casi di perdita dati.

**Documentazione**: `README.md`, `README.it.md`, `docs/restore.md`, `docs/security.md`,
`docs/handbook.it.md` per i tre cambi di comportamento; `CHANGELOG.md`.

---

## Uscita di fase

- Un backup cifrato con un blob `aeadNone` è rifiutato da ogni sorgente e da ogni comando.
- `--overwrite` non cancella dati che il backup non contiene.
- `--continue` non estrae né scrive nulla oltre i filtri richiesti.
