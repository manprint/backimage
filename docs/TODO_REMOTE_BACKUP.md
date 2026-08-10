# TODO: remote backup end-to-end

> **Stato 2026-08-10: implementato come protocollo v2 (`--remote-mode stream`,
> default).** Il client invia solo il flusso tar; archiviazione, chunking,
> compressione, cifratura, layer OCI, dedup e push sono sul server. Dettagli in
> [remote.md](remote.md), [protocol.md](protocol.md) e [security.md](security.md).
> Restano aperti i due punti elencati in "Verifiche non ancora eseguite".

## Obiettivo

Quando viene usato `--remote`, l’applicazione locale deve svolgere solo il
ruolo di client di controllo e di trasporto. Scansione, archiviazione,
chunking, compressione, cifratura, costruzione dei layer OCI e push sul
registry devono essere eseguiti dall’applicazione `backimage` remota.

Il job deve poter creare un backup di 50 GB con circa 1 GB libero sulla
macchina che lo avvia. Non deve quindi essere necessario mantenere localmente
né l’archivio completo né tutti i layer temporanei.

## Stato attuale (protocollo v2, `--remote-mode stream`)

- il client esegue solo scansione e `tar`, inviato in frame da 1 MiB; non crea
  archivio, chunk, layer o spool locali;
- il server esegue chunking, compressione, cifratura, indice dei file, layer
  OCI, `HEAD` di deduplica e push, spoolando un layer per volta in `--work-dir`;
- `StreamProgress` riporta al client stato per ricezione/push/pubblicazione;
- `--server-side-compress` è un alias di questa modalità ed è un errore d'uso
  con `--remote-mode layers`;
- passphrase e key file restano sul client; DEK e dati in chiaro sono visibili
  al server (scelta esplicita, documentata in `docs/security.md`);
- ACL `--allow-repo`, quote, rate limit e token effimeri sono invariati.

Misure eseguite: e2e `test/e2e/phase_08_stream.sh` (256 MiB, TCP e QUIC, spool
client ≤ 4 MiB) e prova di scala 4 GiB (vedi `plan/resume.md`).

## Verifiche non ancora eseguite

1. Campagna reale da 50 GiB con 1 GiB effettivamente liberi sul client.
2. Ripresa a metà stream: v2 non ha checkpoint intra-stream, il retry rilegge la
   sorgente (i layer già pubblicati restano saltati dal `HEAD`). Se serve la
   ripresa per layer, usare `--remote-mode layers`.

## Stato precedente (protocollo v1, ora `--remote-mode layers`)

Il protocollo remoto v1 non raggiunge l'obiettivo:

- il client locale legge i dati, crea l’archivio, divide i chunk, comprime e
  cifra;
- il client costruisce e mantiene localmente i layer OCI temporanei;
- solo dopo la preparazione completa il client invia i layer al server;
- il server esegue il `HEAD`/upload dei blob e pubblica manifest e index nel
  registry;
- `--server-side-compress` è una flag di compatibilità e non abilita alcuna
  compressione sul server.

Di conseguenza lo spazio locale richiesto dipende dalla dimensione complessiva
dei layer prodotti. Con 1 GB libero un backup remoto di 50 GB non è garantito
e può terminare per spazio esaurito.

## Lavoro richiesto

1. Definire un protocollo remoto in cui il client invia un flusso di dati
   grezzi o di chunk non elaborati e il server esegue l’intera pipeline.
2. Spostare sul server remoto archivio, chunking, compressione, cifratura,
   layer OCI, deduplicazione e push sul registry.
3. Trasmettere i dati in streaming con memoria e spazio locale limitati; il
   server può usare il proprio spool e il proprio spazio temporaneo.
4. Inviare dal server al client solo stato, progresso, errori e risultato
   finale. Il client non deve costruire o conservare i layer OCI.
5. Decidere esplicitamente dove risiedono passphrase, chiavi e credenziali
   registry. Il comportamento deve essere documentato, con particolare
   attenzione al fatto che il server remoto vedrà i dati prima della cifratura
   se la cifratura viene eseguita lì.
6. Rendere `--server-side-compress` coerente con il nuovo protocollo oppure
   rimuoverla; non deve più essere una flag senza effetto.
7. Mantenere il controllo ACL (`--allow-repo`) e l’autenticazione del server,
   del client e del registry senza trasferire credenziali permanenti più del
   necessario.

## Trasporto da verificare

Il nuovo flusso deve essere verificato separatamente su entrambi i trasporti:

- **TCP/TLS**: trasferimento continuo di almeno 50 GB, backpressure,
  riconnessione, timeout, interruzione e ripresa senza ricostruire il backup
  localmente;
- **QUIC/TLS**: stesso test funzionale, controllo dell’uso di memoria,
  perdita/rioordino pacchetti, timeout e fallback esplicito quando UDP è
  bloccato;
- comportamento identico per autenticazione, pinning/CA, progressi,
  cancellazione e ripresa;
- nessun percorso TCP o QUIC deve materializzare localmente l’intero backup o
  tutti i layer.

## Criteri di accettazione

- backup remoto di 50 GB completato con 1 GB libero sul client;
- spazio client stabile e indipendente dalla dimensione totale del backup,
  salvo buffer limitati e checkpoint;
- compressione, cifratura e push osservabili nei log del server remoto;
- progressi distinti per ricezione, archiviazione, compressione, cifratura e
  push;
- test automatici e test end-to-end su TCP e QUIC;
- documentazione README aggiornata con architettura, limiti, gestione delle
  chiavi e comandi funzionanti per entrambi i trasporti.
