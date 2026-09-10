# Fase A2 — Confinamento dell'estrazione

**Obiettivo**: nessuna operazione di estrazione agisce su un pathname risolto in un momento
diverso da quello in cui viene usato, e nessuna tocca un oggetto fuori dalla destinazione.

**Rilievi coperti**: A02, A03, A17, A18. **Decisione**: DA-03.

**Precondizione**: A0 chiusa. Il bump a `go1.26.6` porta `archive/tar` corretto (GO-2026-4869), che
è esattamente il parser di questa fase: va misurato prima di riscrivere.

---

## A2.1 Traversal ancorato a descrittori con `os.Root`

**Agente: Opus** (progetto) + **Sonnet** (implementazione)

### Il difetto, verificato

`pkg/archive/extract_unix.go:275-320`, `safeJoin` valida con `os.Lstat` e `filepath.EvalSymlinks`,
poi restituisce una **stringa**. Tutte le operazioni successive riaprono quel pathname:
`createOne` (dalla riga 321), la finalizzazione delle directory (riga 203,
`os.Chmod(d.path, …)`), i metadati, gli xattr, i device. Il commento sulla difesa "`O_NOFOLLOW`"
non corrisponde a nessuna apertura con quel flag. Una modifica concorrente del filesystem invalida
il controllo fra la validazione e l'uso.

### Inventario preciso di ciò che va convertito

Nel file ci sono **12** operazioni `os.*` su pathname (`Chmod`, `Chown`, `Lchown`, `Symlink`,
`Link`, `MkdirAll`, `OpenFile`, `RemoveAll`, `Lstat`) più **cinque chiamate di sistema dirette**,
tutte su pathname:

| Riga | Chiamata | Sostituzione |
| --- | --- | --- |
| 217 | `unix.UtimesNanoAt(unix.AT_FDCWD, d.path, ts, AT_SYMLINK_NOFOLLOW)` | `UtimesNanoAt(dirfd, base, …)` |
| 406 | `unix.Mknod(target, typ\|mode, dev)` | `unix.Mknodat(dirfd, base, …)` |
| 431 | `unix.Lchown(target, hdr.Uid, hdr.Gid)` | `unix.Fchownat(dirfd, base, …, AT_SYMLINK_NOFOLLOW)` |
| 461 | `unix.Lsetxattr(target, rest, v, 0)` | vedi sotto: non esiste variante `*at` |
| 497 | `unix.UtimesNanoAt(unix.AT_FDCWD, target, ts, AT_SYMLINK_NOFOLLOW)` | `UtimesNanoAt(dirfd, base, …)` |

### Cosa `os.Root` copre e cosa no, verificato su go1.26.1

Copre: `Open`, `OpenFile`, `Create`, `Mkdir`, `MkdirAll`, `Lstat`, `Stat`, `Readlink`, `Symlink`,
`Link`, `Remove`, `RemoveAll`, `Rename`, `Chmod`, `Chown`, `Lchown`, `Chtimes`, `OpenRoot`.

**Non** copre: xattr, `Mknod` (device), FIFO, e non espone un metodo `Fd()`. Quindi:

- Il descrittore di directory per le chiamate `*at` si ottiene aprendo la directory **attraverso**
  il `Root`: `dir, err := root.OpenFile(filepath.Dir(name), os.O_RDONLY, 0)` e poi `dir.Fd()`.
  Da lì `Mknodat`, `Fchownat`, `UtimesNanoAt` con il nome base, mai con il pathname completo.
- Per gli xattr non esiste una variante `*at`. La strada praticabile è aprire il file regolare con
  `O_NOFOLLOW` tramite il `Root` e usare `unix.Fsetxattr` sul descrittore. Gli xattr su symlink
  restano **non supportati e rendicontati**: non si può aprire un symlink per scriverci, e il
  ripiego via `/proc/self/fd` su descrittori `O_PATH` non è portabile fra kernel.
- `Root.Chtimes` non ha variante "nofollow": per conservare la semantica odierna
  (`AT_SYMLINK_NOFOLLOW`) i timestamp vanno messi con `UtimesNanoAt` sul dirfd, non con
  `Root.Chtimes`.

Riproduzione della review, senza corsa fra processi: archivio con directory `pivot`, poi symlink
`pivot` verso una directory esterna, con `--overwrite`. Il primo oggetto viene sostituito e il
successivo `os.Chmod` segue il link: il modo della directory esterna passa da `0700` a `0777`.

### Intervento

- Aprire la destinazione una volta con `os.OpenRoot` e svolgere **tutto** il traversal dentro quel
  `*os.Root`: creazione, apertura, `Lstat`, `Chmod`, `Chown`, `Symlink`, `Link`, `Remove`, `Rename`.
- Per ciò che `os.Root` non copre, usare le primitive `*at` con il descrittore della directory
  contenitrice ottenuto come descritto sopra. Nessuna di queste operazioni deve ricevere un
  pathname assoluto ricostruito.
- Conservare l'**identità** dell'oggetto creato (descrittore, o `dev`+`ino` verificati) per la
  finalizzazione dei metadati, invece del pathname: la lista `dirFixes` diventa una lista di
  descrittori o di identità verificate.
- Gestire duplicati e sostituzioni di tipo dentro lo stesso tar senza applicare i metadati del
  primo oggetto al secondo.
- Un `Lstat` in più prima di `Chmod` **non** risolve la corsa: va scritto nel commento, al posto di
  quello attuale che cita `O_NOFOLLOW`.

Riferimento: [Traversal-resistant file APIs](https://go.dev/blog/osroot).

**Ordine dei metadati da preservare**: chown → mode → xattr → timestamp, sugli oggetti veri.

## A2.2 Hardlink: target dentro la radice e solo verso file già ripristinati

**Agente: Sonnet**. **Rilievo**: A02. **Decisione**: DA-03.

### Il difetto, verificato

`pkg/archive/extract_unix.go:383`:

```go
first := filepath.Join(dest, filepath.FromSlash(hdr.Linkname))
if err := os.Link(first, target); err != nil {
	...
	copied, cerr := copyFile(first, target, headerMode(hdr))
```

`hdr.Name` passa da `safeJoin`; `hdr.Linkname` **no**. Con `Linkname="../outside"` il link raggiunge
un file esterno, e le operazioni sui metadati agiscono sullo stesso inode: la review porta il modo
di `outside` da `0600` a `0644` senza privilegi di root. Il fallback di copia apre lo stesso
pathname arbitrario.

### Intervento

- Risolvere sorgente e destinazione **dentro** il `*os.Root` della destinazione.
- Ammettere come target solo un **file regolare già ripristinato e validato in questa corsa**:
  serve un insieme dei nomi materializzati, popolato mentre il tar scorre.
- Rifiutare traversal, target assoluti, componenti symlink intermedi o finali.
- Il fallback di copia applica gli stessi vincoli e non apre pathname arbitrari.
- **DA-03**: se il primo nome non è nell'insieme — perché escluso dai filtri, tagliato da
  `--strip-components`, o non ancora incontrato — l'entry viene **saltata e rendicontata** in
  `stats.Skipped` e `Stats.Errors`, mai copiata dal disco. In `--strict` resta fatale come ogni
  altro errore di entry.
- Gli hardlink "forward" (primo nome successivo nel tar) vanno gestiti esplicitamente: o si rimanda
  la creazione a fine corsa, o si rendicontano come saltati. Scegliere e documentare.

**Cambio di fedeltà da documentare**: oggi un hardlink il cui primo nome è filtrato viene
materializzato come copia leggendo dal disco; dopo il fix non lo è più.

## A2.3 Backslash nei nomi Unix

**Agente: Sonnet**. **Rilievo**: A17.

### Il difetto, verificato

`pkg/archive/entry.go:67-69`:

```go
func CleanPath(p string) string {
	return path.Clean(strings.ReplaceAll(p, "\\", "/"))
}
```

Su Unix `a\b` è il nome legittimo di **un** file e diventa la directory `a` con dentro `b`. Non è
un'evasione — dopo la sostituzione `..\..` diventa `../..` e `safeJoin` lo rifiuta — è corruzione
di dati nel roundtrip.

### Intervento

- La normalizzazione dei backslash appartiene al percorso Windows, non alla funzione condivisa:
  `CleanPath` resta pura su Unix.
- Verificare che il produttore (`pkg/archive`, scansione e scrittura del tar) registri il nome
  byte per byte, e che l'indice lo trasporti identico.
- Test di roundtrip su nomi con backslash, apici, newline, UTF-8 non normalizzato e byte non-UTF-8:
  nome dentro il backup identico al nome sul filesystem.

## A2.4 Windows: riscrittura, non completamento

**Agente: Sonnet**. **Rilievo**: A18, riclassificato da P2 a P1.

### Il difetto, verificato

`pkg/archive/extract_windows.go`, 90 righe in tutto:

- **`Includes`, `Excludes` e `StripComponents` sono ignorati**: su Windows un restore selettivo
  estrae **tutto il backup**. È la stessa classe di A13, ma permanente e silenziosa.
- `tar.TypeLink`, `TypeChar`, `TypeBlock`, `TypeFifo` cadono nel `default` e vengono creati come
  **file regolari vuoti** (per un hardlink `hdr.Size` è 0). Nessun errore, nessuno `Skipped`.
- Nessun equivalente di `safeJoin`: solo un controllo su `h.Name`, nessuna difesa dai
  symlink/junction preesistenti nella destinazione.
- `os.FileMode(h.Mode)` senza mascheratura; nessun `Strict`, nessun `degrade`, nessuna
  finalizzazione dei timestamp.

### Intervento

- Applicare filtri e `--strip-components` con lo stesso codice condiviso del percorso Unix: la
  selezione non è una proprietà del sistema operativo.
- Tipi non supportati: `Skipped` più `Stats.Errors`, fatali in `--strict`. Mai un file vuoto al
  posto di un hardlink o di un device.
- Traversal ancorato con `os.Root`, che su Windows copre anche le junction.
- Documentare in modo esplicito cosa Windows non può ripristinare, invece di lasciarlo dedurre.

## A2.5 CI: la matrice esce da Linux

**Agente: Haiku**

`.github/workflows/ci.yml` ha tre job, tutti `ubuntu-latest`. La review dice esplicitamente che i
risultati Linux non certificano gli altri sistemi, e A2.4 riguarda Windows.

- Job `windows-latest`: `go build`, `go test ./...`, più gli unit test di estrazione.
- Almeno la compilazione per `darwin/amd64` e `darwin/arm64` (già in `build-all`), e i test dei
  package portabili dove eseguibili.

---

## Accettazione della fase

Dalla review, per A02: target con `..`, assoluti, symlink intermedi e finali, link preesistenti,
filesystem differenti, forward link, `--strip-components`. **Nessun contenuto, owner, mode,
timestamp o xattr esterno alla destinazione deve cambiare.** Provare rootless e root in ambiente
isolato.

Per A03: prova deterministica directory→symlink; sostituzioni concorrenti degli antenati; link
verso l'esterno; oggetti con lo stesso nome ripetuti nel tar; conservazione dell'ordine
chown→mode→xattr→timestamp sugli oggetti veri.

Per A17: roundtrip di nomi ostili, byte per byte.
Per A18: restore selettivo su Windows che estrae **solo** ciò che è stato chiesto; tipi non
supportati rendicontati.

**e2e**: `test/e2e/phase_A2.sh` (Unix, rootless e con privilegi separati) e un job Windows che
esegue il caso selettivo.

**Documentazione**: `docs/FIDELITY.md`, `docs/handbook.it.md`, `README*.md` per il cambio di
fedeltà degli hardlink e per i limiti Windows; `CHANGELOG.md`.

---

## Uscita di fase

- Ogni operazione di estrazione è ancorata a un descrittore.
- Nessun percorso di estrazione può toccare un oggetto fuori dalla destinazione, nemmeno tramite
  `Linkname` o sostituzione concorrente.
- I filtri valgono su tutte le piattaforme supportate.
