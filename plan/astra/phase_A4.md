# Fase A4 — Delega remota: lo scope non lo scelgono gli altri

**Obiettivo**: il client non chiede al proprio provider di credenziali nulla che il backup in corso
non richieda, e non spaccia una credenziale permanente per una delega limitata.

**Rilievi coperti**: A06, A07. **Decisione**: DA-02.

---

## A4.1 Scope derivato localmente, non dal messaggio del server

**Agente: Sonnet**. **Rilievo**: A06.

### Il difetto, verificato

`pkg/remote/client.go:285-291`, in `provideToken` (lo `scope` è costruito alla riga 289):

```go
scope := registry.Scope{Repository: request.Repository, Actions: append([]string(nil), request.Actions...)}
token, err := c.client.cfg.Provider.Get(ctx, scope)
```

Repository e azioni arrivano dal messaggio del server e finiscono direttamente in `Provider.Get`.
Nessun controllo che il repository sia quello del backup, nessun controllo che le azioni siano un
sottoinsieme di `pull,push`. Vale anche per le richieste ausiliarie (`handleAux`, `:270`) e per il
rinnovo (`refreshToken`, `:309`, che riusa lo `scope` ricevuto).

Dove prendere lo scope legittimo: il riferimento locale viene risolto con `name.ParseReference` in
`internal/cli/remote_common.go:35` per il percorso remoto. È da lì che repository normalizzato e
azioni vanno derivati e passati alla connessione, invece di essere accettati dal peer.

La review riproduce un provider sintetico che registra la richiesta
`unrelated/repository:delete` e vede il client inoltrarla e scrivere il token sullo stream. Quanto
poi il registry conceda dipende dai privilegi dell'account: il difetto è che la decisione non è
nostra.

### Intervento

- Alla costruzione della connessione, derivare **una volta** dal riferimento scelto localmente:
  repository normalizzato e insieme di azioni autorizzate (`pull` per la lettura, `pull,push` per
  l'upload). Memorizzarli nella `connection`.
- `provideToken` rifiuta **prima** di chiamare `Provider.Get` qualunque richiesta che eccede:
  repository diverso, azione fuori dall'insieme, wildcard, azioni ripetute, cambio di scope in
  corso di sessione.
- La stessa regola su v1, v2, ack iniziali e refresh: un solo punto di validazione, invocato da
  tutti.
- Limitare il numero di scope per sessione e il numero di goroutine di rinnovo (oggi
  `refreshToken` ne avvia una per scope, `client.go:300-305`).

### Accettazione

Server TLS autenticato che chiede: altri repository, `delete`, wildcard, azioni ripetute, cambi di
scope durante la sessione. **Zero chiamate al provider** per le richieste vietate e **nessun token
sul wire**. Il test verifica il conteggio delle invocazioni del provider, non solo l'esito.

## A4.2 Un bearer permanente non viene spacciato per delega

**Agente: Sonnet**. **Rilievo**: A07. **Decisione**: DA-02.

### Il difetto, verificato

`pkg/registry/token.go:196-198`, in `provider.mint`:

```go
if cfg.RegistryToken != "" {
	return &Token{Value: cfg.RegistryToken, ExpiresAt: p.now().Add(24 * time.Hour), Scope: scope}, nil
}
```

Il valore originale viene restituito così com'è, con una scadenza **inventata** di 24 ore e lo scope
richiesto. Non avviene alcuno scambio con l'issuer. Poi `sendToken`
(`pkg/remote/client.go:333-340`) lo inoltra al server etichettandolo con repository, azioni e
`ExpiresAtUnix`: etichette che non cambiano né i privilegi né la durata che il registry verificherà.

### Intervento

- Togliere la scadenza inventata. Un token che non ha una scadenza verificabile ha `ExpiresAt`
  zero e viene marcato come **non delegabile**.
- `sendToken` rifiuta un token non delegabile e l'errore emerge **prima** dell'upload, non a metà.
  Il messaggio nomina il flag di uscita.
- `--forward-static-token`: opt-in esplicito che descrive il cambio di fiducia — il server riceve
  una credenziale piena, non una delega limitata — e va registrato nell'output del comando, non solo
  nella documentazione.
- **Coordinamento obbligatorio**: `sendToken` (`pkg/remote/client.go:334`) considera già
  `token.ExpiresAt.IsZero()` come token *invalido* e restituisce "registry provider returned an
  invalid token". Se si azzera `ExpiresAt` senza toccare quel controllo, l'utente riceve un errore
  che parla di provider difettoso invece di credenziale non delegabile. Servono due stati distinti:
  invalido e non-delegabile, con messaggi diversi.
- Registry solo-Basic: l'incompatibilità deve emergere prima dell'upload, **senza indebolire il
  contratto per aggirarla**.

### Accettazione

Token host-wide e account nominati, in entrambe le modalità remote. Nessuna credenziale permanente
deve essere inviata nel profilo documentato come delega limitata. Con `--forward-static-token`
l'invio avviene e l'output lo dichiara. Registry Basic-only: errore prima dell'upload.

---

## Impatto sull'utenza, da mettere nel changelog

Chi usa `AuthConfig.RegistryToken` (tipicamente CI con un PAT nel docker config) e la modalità
remota vede il backup **fallire subito** invece di funzionare. La via d'uscita è
`--forward-static-token`, con la nota che in quel profilo il server remoto riceve una credenziale
piena. È l'unica rottura deliberata di questa fase.

**Test interni**: `pkg/remote`, `pkg/registry`, con server TLS di test.
**e2e**: `test/e2e/phase_A4.sh` — server remoto che chiede uno scope estraneo, e sessione con token
statico con e senza opt-in.
**Documentazione**: `docs/remote.md`, `README*.md`, `docs/handbook.it.md`, `CHANGELOG.md`.

---

## Uscita di fase

- Il provider di credenziali non viene mai interrogato per qualcosa che il backup non richiede.
- Nessun token permanente lascia il client sotto l'etichetta di delega limitata.
