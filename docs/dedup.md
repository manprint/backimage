# Deduplica incrementale

`backimage backup --dedup` rende economico il backup successivo di dati quasi
immutati. Rimane un'immagine OCI valida: la deduplica è quindi a granularità di
**layer**, non di singolo chunk.

## Come funziona

1. Lo stream tar usa chunking CDC Rabin con parametri riproducibili: minimo
   1 MiB, media 4 MiB, massimo 16 MiB e polinomio `0x3DA3358B4DC173`.
2. I layer si chiudono con un predicato sul digest dei chunk (target predefinito
   64 MiB nella CLI dedup, minimo 1/4 e massimo 4×). Un inserimento non sposta
   tutti i confini successivi come farebbe una soglia fissa.
3. Se la cifratura è attiva, il client riapre `keys.pass.age` o `keys.age` del
   backup più recente usando la passphrase o `--age-identity`; riusa quella
   chiave soltanto se era già in modalità convergente **e** se il backup
   precedente dichiara `encryption.envelopeVersion: 2`. Una chiave che ha
   sigillato con la derivazione nonce precedente alla 0.2.4 è considerata
   bruciata (vedi [security.md](security.md)): viene generata una chiave nuova e
   il backup ricarica tutti i blob **una volta sola**, poi la deduplica riprende
   normalmente. Il messaggio di progresso lo dice esplicitamente.
4. Il livello di compressione viene ereditato dal backup di riferimento quando
   non è stato indicato con `--compression-level`. Un chunk si deduplica solo se
   comprime negli **stessi byte**, e il livello lo decide quanto il codec: un
   default che si muove fra due release ricodificherebbe ogni chunk azzerando la
   dedup senza alcun segnale. Se codec o livello effettivi differiscono da quelli
   del backup precedente, il backup lo segnala; il codec viene segnalato e non
   adottato, perché adottarlo potrebbe tirare dentro `xz` o `lz4` che
   un'immagine eseguibile rifiuta.
5. Il registry riceve `HEAD` per ogni blob: i layer con digest già presenti non
   vengono caricati. L'output JSON riporta `skippedBlobs`, `skippedBytes` e
   `uploadedBytes`.

## Determinismo della compressione

La dedup poggia su un'assunzione non ovvia: lo stesso chunk, con lo stesso codec
e lo stesso livello, deve produrre gli stessi byte su qualunque macchina. Se non
è così il digest del blob cambia, il registry vede un layer nuovo e il tasso di
riuso crolla **in silenzio**.

Lo zstd è configurato con `WithEncoderConcurrency(min(GOMAXPROCS,4))`, quindi il
numero di worker varia con la macchina e con il carico. È stato misurato che
`klauspost/compress` mantiene l'output indipendente da quel valore, perciò la
parallelizzazione resta: `TestZstdOutputIndependentOfWorkerCount` in
`pkg/compress` lo blocca, così un eventuale cambiamento della libreria fa
fallire un test invece di degradare la dedup senza avvisare.
`TestCodecOutputIsReproducible` verifica la riproducibilità su tutti i codec e
livelli.

Resta un fattore fuori dal controllo del tool: un aggiornamento del compressore
può cambiare l'output a parità di livello, perché le librerie di compressione non
garantiscono stabilità dei byte fra versioni. In quel caso il primo backup dopo
l'aggiornamento ricarica i blob una volta, poi la dedup riprende.

I parametri CDC sono nell'`manifest.json` (`chunking.minChunkBytes`,
`targetChunkBytes`, `maxChunkBytes`, `polynomial`). Un nuovo `--dedup` li
eredita dal backup più recente del repository. Le opzioni avanzate
`--dedup-chunk-min`, `--dedup-chunk-avg`, `--dedup-chunk-max` e
`--dedup-polynomial` li forzano; se differiscono, il programma avvisa che non
ci sarà deduplica con i backup precedenti.

Il limite OCI di 118 data layer resta invalicabile. A 110 layer il client passa
a un confine fisso per i rimanenti, registra un warning e scrive
`chunking.boundaryFallback: true` nel manifest.

## Uso

```sh
backimage backup ./tree --repo localhost:5000/acme/data --tag t1 --dedup \
  --passphrase-file ./backup.pass
backimage backup ./tree --repo localhost:5000/acme/data --tag t2 --dedup \
  --passphrase-file ./backup.pass
```

Per backup protetti solo con recipient age, il secondo comando deve poter
aprire il vecchio keyfile:

```sh
backimage backup ./tree --repo registry.example/acme/data --tag t2 --dedup \
  --recipient age1... --age-identity ./age-identity.txt
```

Con `--no-encrypt --dedup` non esistono chiavi o nonce: CDC e layer
content-defined restano attivi.

Per misurare i blob condivisi già raggiungibili dai tag:

```sh
backimage repo stats localhost:5000/acme/data
```

Il comando riporta blob unici, byte effettivamente occupati e byte referenziati
dai tag, senza scaricare i payload dei layer.

## Limiti e sicurezza

La deduplica può rivelare che due backup condividono contenuti; leggere la
sezione dedicata in [security.md](security.md). Non è un confronto onesto con
restic o kopia: loro deduplicano i blocchi nel proprio repository, mentre
backimage conserva layer tar eseguibili da Docker e beneficia della deduplica
per digest offerta dal registry.

La campagna formale da 4 GiB con modifica distribuita dell'1% è descritta in
`test/e2e/phase_10.sh`; il suo criterio è upload del secondo backup sotto il
25% del primo. Non pubblicare percentuali diverse finché quella campagna non è
stata eseguita sull'ambiente di riferimento.
