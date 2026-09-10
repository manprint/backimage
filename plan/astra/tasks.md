# Task ledger — piano astra

Lavoro fuori piano che **non** e' la correzione di un difetto. Ogni voce ha un
ID stabile, non riusato.

| ID | Chiesto da | Stato | Sintesi |
| --- | --- | --- | --- |
| T-A001 | utente, 2026-09-10 | FATTO | pubblicare il lavoro del piano come release **0.5.0**: rinumerazione, changelog riscritto sulla compatibilita' reale, decisione DA-06 |

---

## T-A001 — la release del piano si chiama 0.5.0

**Chiesto**: «l'ultima release e' la v0.4.0 -> pubblica la v0.5.0».

**Contesto.** Il changelog aveva una sola sezione aperta, `[0.4.1] - non ancora
rilasciata`, e ci era finito dentro tutto il piano: A0 fino ad A7. La mappa di
`overview.md` §5 prevedeva invece tre release (0.4.1, 0.5.0, 0.6.0), nessuna
delle quali e' mai stata pubblicata. Quel che esiste e' un solo salto dalla
v0.4.0.

**Fatto.**

1. `0.4.1` → `0.5.0` in tutti i file fuori da `plan/`: 21 fra documentazione,
   README nelle due lingue, `AGENTS.md`, commenti e messaggi in Go. Ogni
   occorrenza significava «la versione in lavorazione», nessuna si riferiva a
   una 0.4.1 pubblicata, che non esiste.
2. Sezione del changelog aperta come `## [0.5.0] - 2026-09-10`.
3. **Preambolo riscritto.** Diceva «il formato dei metadati dell'immagine non
   cambia — manifest, chunk table, indice e blob privato sono quelli della
   0.4.0». Era vero quando conteneva solo A0-A2 ed e' falso adesso: A6.2 porta
   l'envelope a 3, A6.1 il materiale di chiave a schema 2, A6.3 mette il legame
   dentro il blob privato. Il nuovo testo dice cosa cambia, cosa questa
   versione legge ancora (tutto, con le fixture congelate a dimostrarlo), cosa
   una 0.4.0 **non** legge piu' (un backup cifrato nuovo: accetta envelope 1 e
   2 e rifiuta il 3) e cosa costa il primo backup dopo l'aggiornamento (un
   ricaricamento completo per un incrementale cifrato con `--dedup`, perche'
   cambia l'epoca della chiave).
4. `DA-06` in `overview.md` §3, che supera la numerazione di DA-05 e la mappa
   di §5 senza cancellarle, e una nota nella mappa stessa.

**Non fatto**: nessuna riscrittura della storia del piano. Le righe che
nominano la 0.4.1 in `plan/` restano: dicono cosa il piano aveva deciso
allora, ed e' esattamente il loro scopo.
