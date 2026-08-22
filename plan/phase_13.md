# Fase 13 — `repo prune`: ambito e raggruppamento per regex — **milestone v0.4.0**

**Obiettivo**: permettere a `repo prune` di applicare la retention a un sottoinsieme di tag (`--tag-regex`) e di applicarla in modo indipendente a più insiemi in una sola invocazione (`--group-by-regex`). Caso d'uso: un repository che ospita famiglie di backup diverse (`db_1..db_N`, `app_1..app_N`) dove `--keep-last 3` deve significare "3 per famiglia", non "3 in tutto il repository".

**Realtà da tenere presente**: `prune` è l'unico comando distruttivo e irreversibile del progetto. Ogni scelta di questa fase è subordinata a un requisito: **l'utente deve poter stabilire con certezza, prima di premere invio, quali tag verranno cancellati.** Una feature che rende il prune più espressivo lo rende anche più facile da sbagliare; le contromisure (F13.4, F13.5) non sono opzionali.

**Stato di partenza (verificato)**
- `pkg/registry/retention.go:26` `Policy`, `:39` `Apply` — funzione pura, nessun I/O, `now` iniettato.
- `pkg/registry/retention.go:77` `Policy.empty()` → nessuna regola significa nessuna cancellazione. Invariante di sicurezza da preservare.
- `Policy` espone già `KeepHourly/Daily/Weekly/Monthly/Yearly`, implementati e testati ma **non cablati** a flag CLI. Fuori ambito qui, ma la fase non deve romperli.
- `internal/cli/repo.go:208` costruisce la `Policy`; `:227` cancella con `DeleteTag(force=false)` un tag per volta.
- `pkg/registry/adapter_oci.go:139` `DeleteTag` esegue un `ListTags` **per ogni chiamata** e rifiuta se il manifest è referenziato da più di un tag. Delega poi a `DeleteManifest` (`:133`).
- Un solo adapter implementa la cancellazione (`ociAdapter`): la modifica del percorso di cancellazione in F13.5 non ha altri implementatori da adeguare.
- `Policy` è usata **solo** dalla CLI, non da file di configurazione: il refactor del core è libero.
- Harness di test disponibile: registry OCI in-process via `ggcrregistry.New()` + `httptest` (`pkg/registry/adapter_oci_created_test.go:26`).

---

## Decisioni prese (non riaprire senza motivo)

| Decisione | Scelta | Motivo |
|---|---|---|
| Semantica | **entrambe**: `--tag-regex` (ambito) e `--group-by-regex` (partizione) | l'ambito dà la massima verificabilità e consente policy diverse per famiglia; il group-by copre il caso cron in un solo run. Componibili, non esclusivi. |
| Ancoraggio | **full match implicito** su entrambi i flag: il pattern viene incapsulato in `\A(?:…)\z` | la semantica *unanchored* di Go farebbe matchare `db` anche su `app_db_1` e `mydb_x`. Regola unica da ricordare: *il pattern deve coprire il tag intero*. Un pattern incompleto non matcha nulla — il typo è evidente subito invece di allargare silenziosamente l'insieme. |
| Digest condivisi | **pre-check fail-fast** su tutto il remove-set prima di qualunque cancellazione | evita la cancellazione parziale del comportamento attuale (errore a metà loop, registry in uno stato che non corrisponde né al piano né al punto di partenza). |
| Nome del flag di ambito | `--tag-regex` | accanto a `--keep-tag` (glob, regola *keep*) non è ambiguo su chi conserva e chi cancella. |

### Conseguenza dell'ancoraggio sul group-by

Con il full match, la chiave di gruppo si scrive `'([a-z]+)_.*'` e non `'^([a-z]+)_'`: il capture group estrae la chiave, il resto del pattern deve comunque coprire la coda del tag. Effetto collaterale desiderabile: un pattern di group-by scritto male **non matcha nulla** (zero gruppi, zero cancellazioni) invece di raggruppare male.

---

## Invarianti di sicurezza — da codificare come test, non come documentazione

Ognuna corrisponde a un modo concreto di distruggere dati per errore.

1. **La regex non è mai una regola di cancellazione.** `Policy.empty()` **non deve** considerare `Scope` né `GroupBy`. Se li considerasse, `--tag-regex 'db_.*'` da solo, senza alcuna regola di retention, cancellerebbe tutti i tag `db_*`.
2. **I tag fuori ambito non entrano nel conteggio.** La partizione deve avvenire *prima* dell'ordinamento e del conteggio. Se i tag fuori ambito restassero nella lista ordinata, `--tag-regex 'db_.*' --keep-last 3` su un repository con 4 `app_*` più recenti terrebbe i tre `app_*` e cancellerebbe **tutti** i `db_*`. Questo è il bug peggiore possibile della fase.
3. **`Created.IsZero()` resta sempre keep**, dentro e fuori ambito (comportamento attuale, `retention.go:50`).
4. **Nessuna I/O prima della validazione.** Regex non compilabile, `--group-by-regex` senza capture group, pattern vuoto → `KindUsage` prima di `repoAdapter` (la funzione segue già questa convenzione, commento a `repo.go:185`).
5. **Il conteggio dei match è sempre stampato**, anche in JSON: `4 tag su 10 selezionati`. Zero match segnala un typo; N su N segnala una regex più larga del previsto.
6. **Nessuna cancellazione parziale.** Il pre-check di F13.5 gira per intero prima della prima DELETE.
7. `--keep-tag` (glob) resta una regola *keep* che opera **dentro** l'ambito. Nessun conflitto: fuori ambito è già keep.

---

## 13.1 Core puro: ambito e partizione in `Policy`

**Agente: Sonnet** (stesura), **Opus** (revisione delle invarianti)

### File: `pkg/registry/retention.go`

Due campi nuovi in `Policy`, con la documentazione che spiega la semantica di sicurezza:

```go
// Scope restricts which tags the policy may remove. A tag whose name does not
// match is out of scope: it is always kept, and it never counts towards
// KeepLast or any calendar bucket. Nil means every tag is in scope.
Scope *regexp.Regexp
// GroupBy partitions the in-scope tags: each distinct capture is one group and
// the rules run independently inside it, so KeepLast means "N per group". A tag
// that does not match is left ungrouped and kept, like an out-of-scope one.
// Nil means a single group holding every in-scope tag.
GroupBy *regexp.Regexp
```

`empty()` resta **identica** (invariante 1).

`Apply` diventa un involucro sottile e il corpo attuale, da `ordered := …` in poi, si estrae in `applyOne(tags []TagInfo, now time.Time)` non esportato e non modificato nella logica. Il risultato completo vive in un tipo `Plan`, così ciò che si rende e ciò che si cancella provengono da **una sola** risoluzione (e da un solo orologio):

```go
type Plan struct {
	Total, InScope, Ungrouped int
	Groups                    []GroupResult // chiave crescente
	Untouched, Keep, Remove   []TagInfo
}

func (p Policy) Apply(tags []TagInfo, now time.Time) (keep, remove []TagInfo) {
	plan := p.PlanFor(tags, now)
	return plan.Keep, plan.Remove
}

func (p Policy) PlanFor(tags []TagInfo, now time.Time) Plan // regole applicate per gruppo
func (p Policy) Select(tags []TagInfo) Plan                 // solo selezione, nessuna regola
func (p Policy) partition(tags []TagInfo) (Plan, map[string][]TagInfo, []string)
```

`partition` è il passo condiviso da `PlanFor` e `Select`: restringe con `Scope`, raggruppa con `GroupBy` e restituisce i gruppi **in ordine deterministico** (chiave crescente) più i tag intoccabili (fuori ambito o non raggruppabili). Condividerlo è ciò che rende l'anteprima di 13.4 identica per costruzione all'insieme su cui il prune agisce. La firma pubblica di `Apply` non cambia.

Helper di compilazione, esportato perché lo riusano CLI e `repo tags` (F13.4):

```go
// CompileTagPattern compiles a user-supplied pattern anchored to the whole tag
// name: `db_` matches nothing and `db_.*` matches db_1. Anchoring both ends
// removes the substring surprise of Go's default semantics, where `db` would
// also select app_db_1.
func CompileTagPattern(expr string) (*regexp.Regexp, error)
```

Compilare **prima** il pattern grezzo (per restituire un errore con offset corretti e leggibili), poi quello incapsulato. Il wrapping usa un gruppo non catturante `(?:…)` per due ragioni: preserva la precedenza dell'alternanza (`a|b` diventa `\A(?:a|b)\z`, non `\Aa|b\z`) e non altera gli indici dei capture group usati dal group-by.

Estrazione della chiave di gruppo: `FindStringSubmatch`; con il full match il risultato è il tag intero o `nil`. Con più capture group la chiave è la loro concatenazione con un separatore che non può comparire in un tag (`\x00`, come già fa `tagKey`).

Serve anche il verso opposto, per i messaggi: `PatternSource(*regexp.Regexp) string` recupera il pattern che l'utente ha scritto, così l'output mostra `db_.*` e non l'ancorato `\A(?:db_.*)\z`.

### Definition of Done
- [ ] `Apply` mantiene firma e comportamento identici quando `Scope` e `GroupBy` sono `nil` (test di regressione sul comportamento pre-fase)
- [ ] `empty()` invariata, verificata da un test dedicato con solo `Scope` valorizzato
- [ ] `partition` deterministica: due invocazioni sullo stesso input danno lo stesso ordine di gruppi
- [ ] RE2 documentato: nessun lookahead né backreference; nessun rischio ReDoS per costruzione

### Test (`pkg/registry/retention_test.go`)
- tabellari: ambito, group-by, ambito + group-by combinati, group-by senza match, ambito senza match
- **invariante 1**: `Policy{Scope: re}` senza alcuna regola → `remove` vuota
- **invariante 2**: 4 `app_*` più recenti + 4 `db_*` più vecchi, `Scope='db_.*'`, `KeepLast=3` → rimosso **esattamente** `db_1`, nessun `app_*` toccato. Questo test è il presidio del bug peggiore della fase.
- **invariante 3**: un tag con `Created` zero dentro l'ambito → keep
- estensione di `TestRetentionPartitionsAreCompleteAndDisjoint` (`retention_test.go:55`) a tutte le combinazioni di ambito e group-by
- indipendenza fra gruppi: il risultato del gruppo A non cambia al variare del contenuto del gruppo B
- full match: `Scope='db_'` → zero match; `Scope='db_.*'` → i soli `db_*`; `Scope='.*db.*'` → include `app_db_1`
- alternanza: `Scope='db_.*|app_.*'` seleziona entrambe le famiglie (verifica del wrapping `(?:…)`)

---

## 13.2 Flag CLI e validazione

**Agente: Sonnet**

### File: `internal/cli/repo.go`

```go
prune.Flags().String("tag-regex", "",
	"restrict the prune to tag names matching this regex; the pattern must match the whole tag, and non-matching tags are never touched")
prune.Flags().String("group-by-regex", "",
	"partition tags by the capture group(s) of this regex and apply the rules independently inside each group, e.g. '([a-z]+)_.*' (whole-tag match, at least one capture group required)")
```

In `runRepoPrune`, **prima** di `repoAdapter` (`repo.go:210`):
- pattern non compilabile → `KindUsage`, messaggio con il pattern citato e l'errore RE2
- pattern presente ma vuoto o solo spazi → `KindUsage` (un `--tag-regex ''` che valesse "tutto" sarebbe una trappola)
- `--group-by-regex` con `NumSubexp() == 0` → `KindUsage`: senza capture group ogni tag sarebbe un gruppo a sé e `--keep-last 3` conserverebbe tutto, silenziosamente
- i due flag **compongono** (ambito prima, raggruppamento dentro l'ambito): nessun controllo di mutua esclusione

Aggiornare `Long` del comando con due esempi:

```
  # dei backup del database tieni i 3 più recenti, gli altri tag non li toccare
  backimage repo prune ghcr.io/me/dumps --tag-regex 'db_.*' --keep-last 3 --dry-run

  # tieni i 3 più recenti per ogni famiglia (db_*, app_*, …) in un solo passaggio
  backimage repo prune ghcr.io/me/dumps --group-by-regex '([a-z]+)_.*' --keep-last 3 --dry-run
```

E aggiungere al `Long` la frase sull'ancoraggio: *"Il pattern deve corrispondere al tag intero: `db_` non seleziona nulla, `db_.*` seleziona `db_1`."*

### Definition of Done
- [ ] `--help` cita l'ancoraggio full-match su entrambi i flag
- [ ] nessuna chiamata di rete su pattern invalido (test con adapter non raggiungibile: deve fallire in usage, non in network)
- [ ] exit code 2 (`KindUsage`) per ogni errore di validazione

### Test (`internal/cli/repo_prune_test.go`)
- pattern invalido (`'db_['`) → `KindUsage`, e il messaggio contiene il pattern
- `--group-by-regex 'db_.*'` (zero capture group) → `KindUsage`
- `--tag-regex ''` esplicito → `KindUsage`
- stesso schema di `TestRepoPruneRejectsBothDurationFlags` (`repo_prune_test.go:46`)

---

## 13.3 Output verificabile: breakdown per gruppo

**Agente: Sonnet** (stesura), **Opus** (revisione del testo umano)

È il pezzo che realizza il requisito della fase: rendere l'insieme da cancellare leggibile *prima* di cancellarlo.

### File: `internal/cli/repo.go` (`prunePlanText`, `activePruneRules`)

Testo umano:

```
regole attive: mantieni i 3 più recenti
ambito: --tag-regex 'db_.*|app_.*' — 8 tag su 10 selezionati
gruppi: 2 (--group-by-regex '([a-z]+)_.*')

gruppo "app" — 4 tag: 3 conservati, 1 da eliminare
  app_1	2026-08-10T21:08:47Z	sha256:ab…
gruppo "db" — 4 tag: 3 conservati, 1 da eliminare
  db_1	2026-08-10T20:11:03Z	sha256:cd…

2 tag da eliminare (dry-run, nessuna modifica al registry), 8 conservati.
ripetere senza --dry-run e con --yes per applicare.
```

Regole di rendering:
- riga `ambito:` presente solo se `--tag-regex` è stato passato; riga `gruppi:` solo con `--group-by-regex`. Senza nessuno dei due l'output resta **identico** a quello attuale.
- zero match → riga esplicita `nessun tag corrisponde al pattern: nulla da eliminare` (segnale di typo, non un successo silenzioso)
- gruppi in ordine di chiave crescente, come li restituisce `partition`

JSON — i campi attuali restano al loro posto per non rompere i consumatori esistenti (`dryRun`, `kept`, `remove` come oggi a `repo.go:232`), i nuovi si aggiungono:

```json
{
  "dryRun": true,
  "kept": 8,
  "remove": [ … ],
  "scope":   {"tagRegex": "db_.*|app_.*", "matched": 8, "total": 10},
  "groupBy": {"regex": "([a-z]+)_.*", "groups": 2, "ungrouped": 0},
  "groups": [
    {"key": "app", "tags": 4, "kept": 3, "remove": [ … ]},
    {"key": "db",  "tags": 4, "kept": 3, "remove": [ … ]}
  ]
}
```

### Definition of Done
- [ ] senza i flag nuovi, testo umano e JSON byte-identici a prima della fase (test di regressione con golden output)
- [ ] `TestPrunePlanText` (`repo_prune_test.go:65`) esteso ai casi con ambito e gruppi
- [ ] il conteggio `matched/total` compare in entrambi i formati

---

## 13.4 Anteprima read-only: `repo tags --tag-regex`

**Agente: Sonnet**

Il miglior rapporto costo/beneficio della fase in termini di sicurezza: la stessa regex, lo stesso matcher (`registry.CompileTagPattern`), su un comando che **non può cancellare nulla**. L'utente verifica l'insieme prima di passare il pattern a `prune`.

### File: `internal/cli/repo.go` (`runRepoTags`, `:130`)

```
prune-preview:
  backimage repo tags ghcr.io/me/dumps --tag-regex 'db_.*'
```

- riusa `CompileTagPattern`: se il matcher divergesse fra i due comandi l'anteprima diventerebbe una bugia, quindi la funzione **deve** essere condivisa, non duplicata
- stampa il conteggio `4 tag su 10` come in `prune`
- `--group-by-regex` opzionale anche qui: mostra i gruppi che `prune` vedrebbe, senza applicare regole

### Definition of Done
- [ ] `repo tags --tag-regex X` e `repo prune --tag-regex X --dry-run` selezionano lo **stesso** insieme (test che confronta i due output sullo stesso registry finto)
- [ ] `repo tags` senza il flag non cambia comportamento
- [ ] documentato nel `Long` di `prune` come passo di verifica raccomandato

---

## 13.5 Pre-check dei digest condivisi e cancellazione per digest

**Agente: Opus** (è il percorso distruttivo)

Chiude un bug latente già presente e reso più probabile dal group-by: famiglie diverse possono contenere dump identici, quindi lo **stesso manifest** con più tag.

### File: `internal/cli/repo.go` (loop di cancellazione, `:227`)

Il piano è già completamente noto prima della prima DELETE — `ListTags` ha restituito tag e digest — quindi il controllo si fa in locale, senza toccare l'interfaccia `Adapter`:

1. indicizzare per digest **tutti** i tag del repository e i soli tag del remove-set
2. per ogni digest presente nel remove-set: se il repository ha per quel digest almeno un tag **non** nel remove-set, il manifest è condiviso con un tag che la policy vuole conservare → **rifiuto**, elencando le coppie coinvolte. Nessuna DELETE eseguita.
3. altrimenti cancellare **una volta per digest distinto** con `DeleteManifest(repo.Digest(d))`

Messaggio di rifiuto:

```
il prune si fermerebbe a metà: 1 manifest da eliminare è referenziato anche da tag conservati
  sha256:ab…  da eliminare: db_1  conservati: app_1
nessun tag è stato eliminato. Restringere il pattern, oppure eliminare
insieme i tag che condividono il manifest con `repo rm --force`.
```

Due effetti collaterali del punto 3, entrambi desiderabili:
- **elimina l'N+1 di rete**: `DeleteTag` faceva un `ListTags` per ogni tag (`adapter_oci.go:144`); su 50 tag erano 50 listing inutili
- **elimina il falso positivo**: quando *tutti* i tag di un manifest sono nel remove-set, `DeleteTag(force=false)` rifiuterebbe comunque (vede `len(shared) > 1`), pur trattandosi del caso legittimo. La cancellazione per digest lo gestisce correttamente senza ricorrere a `force`.

`ociAdapter` è l'unico implementatore della cancellazione e `DeleteTag` delega già a `DeleteManifest` (`adapter_oci.go:157`): nessuna capability persa, nessun altro adapter da adeguare. `repo rm` resta invariato.

### Definition of Done
- [ ] su collisione: exit non-zero, **zero** DELETE inviate al registry (verificato contando le richieste sul registry finto)
- [ ] `DeleteManifest` chiamata **una volta** per digest distinto
- [ ] il caso "tutti i tag del manifest sono nel remove-set" riesce senza `--force`
- [ ] il pre-check gira anche senza `--tag-regex`/`--group-by-regex`: è un miglioramento del prune in generale

### Test (`internal/cli/` + `test/e2e/`)

Il registry in-process di ggcr indicizza un manifest **due volte**, per tag e per digest, e una DELETE per digest cancella solo la chiave del digest: il tag resta nell'elenco. Su quel finto registry la sparizione del tag non è quindi osservabile, e assumerla renderebbe il test verde per il motivo sbagliato. La verifica si divide:

- **unit** (`internal/cli`): il finto registry viene avvolto da un handler che registra le richieste DELETE ricevute. Si asserisce ciò che il prune *fa*, che è esattamente l'oggetto delle regole di sicurezza — quante DELETE, per quali digest, e se ne parte qualcuna prima della validazione del piano:
  - due tag sullo stesso manifest, uno nel remove-set e uno fuori → rifiuto e **zero** DELETE inviate;
  - due tag sullo stesso manifest entrambi nel remove-set → **una sola** DELETE;
  - `--tag-regex 'db_.*'` → una sola DELETE, per il digest di `db_1`, e nessuna che riguardi la famiglia fuori ambito;
  - `--tag-regex` senza regole → zero DELETE.
- **e2e** (`test/e2e/phase_13.sh`): `registry:2` con `REGISTRY_STORAGE_DELETE_ENABLED=true`, backup reali con `--created` fissato. Qui il tag sparisce davvero da `/v2/<name>/tags/list`, e si verifica che i tag esclusi dal selettore siano ancora tutti presenti.

---

## 13.6 Documentazione

**Agente: Haiku** (stesura), **Opus** (revisione)

- `internal/cli/repo.go`: `Long` di `prune` e di `tags` con i nuovi flag, l'ancoraggio e i due esempi
- `docs/cli.md`: rigenerato (gate GS-12.3 lo confronta col committato)
- `docs/registries.md`: nuova sezione "Cancellazione e manifest condivisi" (cancellazione per digest, pre-check, GC dei blob a parte). Nota: `docs/retention.md` è un deliverable della fase 11, il cui gate è ancora aperto; questa fase non lo crea
- `README.md`: la riga `repo prune` nella tabella dei comandi menziona il prune per famiglia
- `CHANGELOG.md`: sezione `[0.4.0]` con **Added** (i due flag, l'anteprima su `repo tags`) e **Fixed** (cancellazione parziale su digest condivisi)

### Definition of Done
- [ ] ogni blocco di esempio della documentazione è stato **eseguito**
- [ ] `make docs-check` verde (nessun flag citato che non esista)
- [ ] il CHANGELOG dichiara il cambio del percorso di cancellazione (da per-tag a per-digest) come nota di comportamento

---

## Gate di fase 13 — **MILESTONE v0.4.0**

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/registry/... ./internal/cli/...` | ≥ 85 % su `retention.go` (è codice puro: nessuna scusa) |
| **GS-13.1** | test invariante 1 | `Policy` con solo `Scope` → `remove` vuota |
| **GS-13.2** | test invariante 2 | ambito `db_.*` + `KeepLast=3` con `app_*` più recenti → nessun `app_*` in `remove` |
| **GS-13.3** | `TestRetentionPartitionsAreCompleteAndDisjoint` esteso | keep ∪ remove = input, keep ∩ remove = ∅, su ogni combinazione di ambito e group-by |
| **GS-13.4** | golden output senza i flag nuovi | testo umano e JSON identici a v0.3.0 |
| **GS-13.5** | `repo tags --tag-regex X` vs `repo prune --tag-regex X --dry-run` | stesso insieme selezionato |
| **GS-13.6** | e2e digest condivisi su registry in-process | rifiuto con zero DELETE inviate |
| **GS-13.7** | ogni nuovo flag con `--help` | exit 0, l'ancoraggio full-match è citato |
| GS-12.2 | `make docs-check` | exit 0 |
| GS-12.3 | `docs/cli.md` rigenerato | nessuna differenza rispetto al committato |
| G11 | revisione Opus del percorso distruttivo (13.5) + tag `v0.4.0` | approvazione in `resume.md` |

**Deliverable documentali**: `docs/cli.md` rigenerato, `docs/registries.md` aggiornato, `CHANGELOG.md` sezione `0.4.0`, `README.md` tabella comandi.

**Rischi noti**
- **Il rischio dominante è l'invariante 2**: partizionare dopo l'ordinamento invece che prima produce un prune che cancella esattamente l'insieme complementare a quello richiesto, e lo fa senza errori. GS-13.2 esiste solo per questo; non va reso più permissivo per far passare un refactor.
- Il full match è una scelta di sicurezza che va contro l'abitudine a grep: chi scrive `--tag-regex 'db_'` non vede match e può concludere che il flag sia rotto. Il messaggio di zero match deve suggerire l'ancoraggio in modo esplicito, non limitarsi a dire "0 tag".
- Il group-by rende plausibili i digest condivisi fra gruppi (dump identici di sorgenti diverse). Senza 13.5 la fase introduce un percorso realistico verso una cancellazione parziale: **13.5 non è separabile da 13.1–13.3.**
- Il passaggio da cancellazione per-tag a cancellazione per-digest cambia il comportamento osservabile su registry che disabilitano la DELETE: l'errore ora arriva una volta sola invece di una per tag. Va verificato che il messaggio di `DeleteManifest` (`adapter_oci.go:134`, cita `REGISTRY_STORAGE_DELETE_ENABLED`) resti raggiungibile e leggibile.

---

## Esito dell'esecuzione — 2026-08-22

Implementata per intero (13.1–13.6). Misure rilevate, non stimate.

### Codice

| Dove | Cosa |
|---|---|
| `pkg/registry/retention.go` | `Policy.Scope`/`GroupBy`, `Plan`, `GroupResult`, `PlanFor`, `Select`, `partition`, `applyOne`, `groupKeyFor`, `CompileTagPattern`, `PatternSource`, `Plan.Selected`, `Plan.finish` |
| `internal/cli/repo.go` | `addTagSelectorFlags`, `tagSelectors`, `compileTagFlag`, `pruneDelete`, `pruneDigests`, `sharedManifestConflicts`, `pruneJSON`, `printTagRows`, `tagSelectionSummary`; `runRepoTags` e `runRepoPrune` riscritti su `Plan` |
| `pkg/registry/retention_test.go` | 15 test nuovi sulle invarianti e sui selettori; `TestRetentionPartitionsAreCompleteAndDisjoint` affiancato dalla variante con selettori |
| `internal/cli/repo_selector_test.go` | 22 test nuovi: validazione flag, rendering, concordanza dei messaggi, manifest condivisi, fallimento a metà piano, integrazione su registry in-process con sonda sulle DELETE |
| `test/e2e/phase_13.sh` | e2e su `registry:2`, 15 asserzioni |

### Gate

| Gate | Criterio | Esito |
|---|---|---|
| G1–G4 | `make fmt vet lint build` | **verde** |
| G5–G6 | `make test race` | **verde** |
| G7 | ≥ 85 % su `retention.go` | **97,6 %** (123/126 statement). `pkg/registry` 79,7 %, `internal/cli` 81,0 % complessivi |
| G8 | `make e2e PHASE=13` | **verde**, 15 asserzioni |
| GS-13.1 | selettore senza regola → `remove` vuota | **verde** (`TestScopeAloneRemovesNothing`, più la controprova su registry reale in `TestPruneTagRegexWithoutRuleDeletesNothing` e nell'e2e) |
| GS-13.2 | ambito `db_.*` + `KeepLast=3` con `app_*` più recenti | **verde** (`TestScopeDoesNotBorrowKeepSlotsFromOtherTags`) |
| GS-13.3 | partizione completa e disgiunta su ogni combinazione | **verde** (`TestRetentionPartitionsStayCompleteWithSelectors`, 400 casi pseudocasuali con seed fisso, tag senza data e tag non raggruppabili inclusi) |
| GS-13.4 | output invariato senza i flag nuovi | **verde** (`TestPrunePlanTextWithoutSelectorsIsUnchanged` confronta il testo golden byte per byte; `TestPruneJSONKeepsTheOldFieldsAndAddsTheBreakdown` verifica che nessun campo nuovo compaia senza selettore; `TestRepoTagsWithoutSelectorsListsEverything` per il listato) |
| GS-13.5 | `repo tags --tag-regex` ≡ `repo prune --tag-regex --dry-run` | **verde** a tre livelli: condivisione di `partition` per costruzione, `TestSelectMatchesWhatPlanForReaches` sul core, `TestRepoTagsPreviewMatchesWhatPruneWouldRemove` sui due comandi, e confronto degli insiemi nell'e2e |
| GS-13.6 | rifiuto su manifest condiviso con zero DELETE | **verde** (`TestPruneRefusesSharedManifestWithoutDeletingAnything` conta le richieste; l'e2e verifica che il registry sia invariato) |
| GS-13.7 | `--help` cita l'ancoraggio | **verde** (`TestTagSelectorFlagsAreSharedByPruneAndTags`) |
| GS-12.2 | `make docs-check` | **verde** |
| GS-12.3 | `docs/cli.md` rigenerato | **verde** (`scripts/generate-cli-docs.sh`) |
| G11 | revisione indipendente del percorso distruttivo | **aperto** — vedere sotto |

`make check` non esce 0 in questo ambiente: `proto-check` (GS-08.9) richiede `protoc` e `protoc-gen-go`, assenti. Verificato che il target falliva identicamente su albero pulito, prima di questa fase: è un limite d'ambiente, non una regressione.

### Deviazioni dal piano

1. **Il finto registry non può mostrare la sparizione del tag.** Il registry in-process di ggcr indicizza il manifest per tag e per digest separatamente, quindi una DELETE per digest lascia in piedi la voce del tag. I test unitari asseriscono perciò le richieste DELETE inviate (quante, per quali digest, se qualcuna parte prima della validazione) e la sparizione effettiva del tag è verificata nell'e2e su `registry:2`. Le asserzioni sono più strette di quelle previste dal piano, non più larghe.
2. **`repo tags --json` mantiene la forma di array.** Il piano chiedeva il conteggio dei match "in entrambi i formati"; trasformare l'array in un oggetto avrebbe rotto i consumatori esistenti. Il conteggio compare nell'output umano, e in JSON è la lunghezza dell'array. `repo prune --json` espone invece `scope.matched` e `scope.total`, perché lì i campi nuovi si aggiungono senza cambiare forma.
3. **Aggiunta non prevista dal piano: il resoconto di un'interruzione a metà.** Il pre-check elimina la causa prevedibile di uno stop a metà ciclo, ma una sequenza di DELETE HTTP non è atomica: un errore di rete sul terzo di cinque manifest lascia i primi due eliminati. L'errore dichiara ora fino a dove è arrivato (`N manifest su M erano già stati eliminati`), perché su un comando distruttivo lo stato del repository non può restare indeterminato. Coperto da `TestPruneReportsHowFarItGotWhenADeleteFails` e documentato in `docs/registries.md`.
4. **`--group-by-regex` senza gruppi di cattura è un errore d'uso della CLI, non della libreria.** A livello di `Policy` la chiave diventa il tag intero (ogni tag un gruppo, quindi nulla da eliminare): comportamento definito e testato, ma inutile come policy, per questo la CLI lo rifiuta con un messaggio che spiega il perché.

### Cosa resta aperto

- **G11**: la revisione indipendente del percorso distruttivo non è auto-certificabile dal codice, come già registrato per la fase 10. Il gate resta aperto fino a una revisione umana o di un secondo modello su `pruneDelete`, `sharedManifestConflicts` e `pruneDigests`.
- **GS-08.9 / `proto-check`**: sbloccabile installando `protoc` e `protoc-gen-go`; indipendente da questa fase.
- **`--keep-hourly/daily/weekly/monthly/yearly`** restano implementati in `Policy` e privi di flag CLI. Ora si combinano correttamente con i selettori (coperti da `TestRetentionPartitionsStayCompleteWithSelectors`), ma esporli è lavoro di un'altra fase.
