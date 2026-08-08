# Fase 01 — `pkg/archive`: fedeltà integrale dei metadati

**Obiettivo**: leggere un albero di file e produrre un tar PAX che, ripristinato, sia **indistinguibile** dall'originale — ownership, permessi, xattr, ACL, capabilities, hardlink, device, timestamp al nanosecondo.

**Perché è la fase più delicata**: un errore qui non si nota finché non serve il backup. Ogni bug è silenzioso. Per questo la fixture ostile (01.2) viene **prima** dell'implementazione.

**Riferimento decisioni**: D07, D08, D09, D10.

---

## 01.1 Modello `Entry`, interfacce, doc.go

**Agente: Sonnet**

### File: `pkg/archive/doc.go`

15 righe che dichiarano: formato PAX, invariante di determinismo dell'ordine, cosa è preservato e cosa no per piattaforma.

### File: `pkg/archive/entry.go`

```go
// EntryType classifies a filesystem object.
type EntryType uint8

const (
	TypeRegular EntryType = iota
	TypeDir
	TypeSymlink
	TypeHardlink
	TypeCharDevice
	TypeBlockDevice
	TypeFifo
)

// Entry is the platform-independent description of one filesystem object.
type Entry struct {
	Path       string            // slash-separated, relative to the archive root, never absolute, never contains ".."
	Type       EntryType
	Size       int64             // 0 for anything but TypeRegular
	Mode       os.FileMode       // permission bits + setuid/setgid/sticky
	UID, GID   int
	Uname      string            // may be empty when the name cannot be resolved
	Gname      string
	ModTime    time.Time         // nanosecond precision
	AccessTime time.Time
	ChangeTime time.Time
	LinkTarget string            // symlink target, or path of the first hardlink occurrence
	DevMajor   int64
	DevMinor   int64
	Xattrs     map[string][]byte // raw values; keys include the namespace, e.g. "user.foo", "system.posix_acl_access"
}

// Validate reports whether the entry is internally consistent and safe to extract.
func (e *Entry) Validate() error
```

`Validate` deve rifiutare: path assoluti, path contenenti `..`, path vuoti, `Size != 0` su tipi non regolari, `LinkTarget` vuoto su symlink/hardlink, chiavi xattr vuote.

### File: `pkg/archive/archive.go`

```go
// Options controls archiving behaviour.
type Options struct {
	Strict        bool     // any read error aborts the operation (default true)
	FollowSymlink bool     // default false: symlinks are archived, not followed
	OneFileSystem bool     // do not cross mount points
	Excludes      []string // glob patterns matched against Entry.Path
	NumericOwner  bool     // do not resolve Uname/Gname
	PreserveACLs  bool     // default true
	PreserveXattrs bool    // default true
}

// Stats accumulates what happened during a walk.
type Stats struct {
	Files, Dirs, Symlinks, Hardlinks, Devices, Fifos, Skipped int64
	BytesRaw int64
	Errors   []error // populated only when Strict is false
}

// Writer streams a deterministic PAX tar of the given roots.
type Writer interface {
	// AddRoot archives one root path. Roots are processed in the order given.
	AddRoot(ctx context.Context, root string) error
	// Close flushes the tar trailer and returns the accumulated statistics.
	Close() (Stats, error)
	// Entries returns the entries emitted so far, in emission order.
	Entries() []Entry
}

// NewWriter builds a Writer that writes to w.
func NewWriter(w io.Writer, opts Options) Writer

// Reader reads a PAX tar produced by this package.
type Reader interface {
	// Next advances to the next entry. Returns io.EOF at the end.
	Next() (*Entry, io.Reader, error)
}

// NewReader builds a Reader over r.
func NewReader(r io.Reader) Reader

// Extractor materialises entries onto the filesystem.
type Extractor interface {
	// Extract consumes a tar stream and writes it under dest.
	Extract(ctx context.Context, r io.Reader, dest string) (Stats, error)
}

// NewExtractor returns the platform extractor.
func NewExtractor(opts ExtractOptions) Extractor

// ExtractOptions controls restore behaviour.
type ExtractOptions struct {
	PreserveOwner  bool     // default true; requires privileges
	PreserveXattrs bool     // default true
	Overwrite      bool     // default false: existing files cause an error
	Includes       []string // if non-empty, only matching paths are extracted
	Excludes       []string
	Strict         bool     // default true
}
```

### Definition of Done
- [ ] i tipi compilano, `Validate` ha 8 test di casi negativi
- [ ] `go vet` e lint verdi

---

## 01.2 Generatore di fixture ostili

**Agente: Sonnet** — *da fare PRIMA dell'implementazione*

### File: `test/fixtures/tree.go`

```go
// Package fixtures builds filesystem trees used by archive round-trip tests.
package fixtures

// Feature flags select which hostile cases to include.
type Feature uint32

const (
	FeatBasic      Feature = 1 << iota // regular files, dirs, empty dir, nested dirs
	FeatPerms                          // 0000, 0777, setuid, setgid, sticky
	FeatSymlinks                       // relative, absolute, dangling, symlink to dir
	FeatHardlinks                      // 2 and 3 links to the same inode
	FeatXattrs                         // user.* xattrs, empty value, 4KiB value, binary value
	FeatACLs                           // POSIX ACL via system.posix_acl_access
	FeatCaps                           // security.capability
	FeatDevices                        // char and block devices (requires root)
	FeatFifos
	FeatOwnership                      // files owned by uid/gid != current (requires root or userns)
	FeatNames                          // unicode, emoji, spaces, newline, 250-byte name, 4096-byte path
	FeatSparse                         // sparse file with a 64 MiB hole
	FeatTimes                          // mtime with nanoseconds, mtime in 1970, mtime in 2200
	FeatBigFile                        // 128 MiB file (skipped in short mode)
)

// Build materialises the tree under dir and returns a manifest describing
// exactly what was created, for later comparison.
func Build(t *testing.T, dir string, feats Feature) *Manifest

// RequiresRoot reports which of the requested features need privileges.
func RequiresRoot(feats Feature) Feature
```

### File: `test/fixtures/compare.go`

```go
// CompareTrees walks both trees and reports every difference in metadata.
// It is the single source of truth for "identical" in this project.
func CompareTrees(t *testing.T, want, got string, opts CompareOptions) []Difference

// CompareOptions relaxes specific checks when the platform cannot support them.
type CompareOptions struct {
	IgnoreOwner      bool
	IgnoreXattrs     bool
	IgnoreACLs       bool
	IgnoreAccessTime bool // atime changes just by reading; ignored by default
	IgnoreCtime      bool // ctime cannot be restored; always ignored
}

// Difference describes one mismatch in a human-readable form.
type Difference struct {
	Path  string
	Field string // "mode", "uid", "mtime", "xattr:user.foo", "content", "hardlink-group", …
	Want  string
	Got   string
}
```

**Regola**: `CompareTrees` deve confrontare **anche** l'appartenenza ai gruppi di hardlink (stessi inode nell'originale ⇒ stessi inode nella copia) e il **contenuto** dei file (SHA-256).

`IgnoreAccessTime` è `true` per default e `IgnoreCtime` è sempre `true`: sono gli unici due allentamenti ammessi, e vanno documentati in `docs/FIDELITY.md`.

### Definition of Done
- [ ] `Build` crea l'albero e `CompareTrees(dir, dir)` restituisce zero differenze
- [ ] copiare l'albero con `cp -a` e confrontare produce zero differenze
- [ ] copiare l'albero con `cp -r` (che perde i metadati) produce **almeno 10** differenze — questo dimostra che il comparatore non è cieco
- [ ] `RequiresRoot(FeatDevices|FeatOwnership)` restituisce entrambi i flag

---

## 01.3 Lettura metadati Unix

**Agente: Sonnet**

### File: `pkg/archive/meta_unix.go` (`//go:build unix`)

```go
// readMeta fills the platform-specific fields of e from fi and path.
func readMeta(path string, fi os.FileInfo, opts Options, e *Entry) error

// readXattrs returns all extended attributes of path, following no symlinks.
func readXattrs(path string) (map[string][]byte, error)

// resolveOwner returns the user and group names for uid/gid, cached.
func resolveOwner(uid, gid int) (uname, gname string)
```

### Prescrizioni tecniche

- xattr: `unix.Llistxattr` / `unix.Lgetxattr` (varianti `L`: **mai** seguire i symlink). Gestire `ERANGE` con ri-allocazione del buffer in ciclo (max 3 tentativi, poi errore).
- Namespace da leggere: **tutti** quelli restituiti da `Llistxattr`. Non filtrare `system.*` né `security.*`: contengono ACL, capabilities e label SELinux. Filtrare solo se `opts.PreserveXattrs == false`.
- ACL POSIX: **nessuna libreria dedicata**. Vengono automaticamente da `system.posix_acl_access` e `system.posix_acl_default`. Documentarlo in `doc.go` per evitare che qualcuno aggiunga una dipendenza inutile.
- macOS: xattr richiede `unix.Lgetxattr` con `options=XATTR_NOFOLLOW`; l'attributo `com.apple.ResourceFork` va incluso; ACL macOS (NFSv4) **non** sono in xattr → non supportate, va scritto in `docs/FIDELITY.md`.
- Timestamp: da `unix.Stat_t.Mtim/Atim/Ctim` per avere i nanosecondi (`fi.ModTime()` li ha già su Linux, ma serve `Atim`).
- `resolveOwner`: cache in `sync.Map`; su fallimento di `user.LookupId` lasciare la stringa vuota, **non** è un errore.
- Device: `DevMajor = unix.Major(st.Rdev)`, `DevMinor = unix.Minor(st.Rdev)`.
- Socket (`S_IFSOCK`): saltato, incrementa `Stats.Skipped`, warning una sola volta.

### Test richiesti (`pkg/archive/meta_unix_test.go`)
- xattr con valore vuoto, valore binario con `\x00`, valore da 4 KiB;
- `ERANGE` simulato scrivendo un xattr grande dopo una prima lettura (test tollerante: verifica solo il valore finale);
- symlink con xattr sul target: l'entry del symlink **non** deve ereditare gli xattr del target;
- setuid/setgid/sticky preservati in `Entry.Mode`;
- file con mtime nel 1970 e nel 2200.

### Definition of Done
- [ ] copertura `pkg/archive` (file unix) ≥ 80 %
- [ ] i test passano da utente non privilegiato (le feature che richiedono root sono gated)

---

## 01.4 Writer tar PAX

**Agente: Sonnet**

### File: `pkg/archive/writer.go`

### Prescrizioni tecniche

1. **Formato**: `tar.Header.Format = tar.FormatPAX` sempre. Mai `FormatGNU`, mai `FormatUSTAR`.
2. **xattr**: scrivere in `hdr.PAXRecords` con chiave `SCHILY.xattr.<nome>` e valore **grezzo** (la stdlib gestisce l'escaping UTF-8 dei record PAX; per valori binari usare la codifica byte-per-byte così com'è — Go la accetta perché i record PAX sono length-prefixed).
   - Test obbligatorio: un valore xattr con byte `\x00` e `\n` sopravvive al round-trip.
3. **Hardlink**: mappa `map[hardlinkKey]string` dove `hardlinkKey{Dev, Ino uint64}`. Solo per file con `Nlink > 1`. La prima occorrenza è `TypeRegular` con i dati; le successive sono `tar.TypeLink` con `Linkname` = path della prima. Ordine deterministico garantito dal punto 5.
4. **Ordinamento deterministico**: il walk ordina le voci di ogni directory con `sort.Strings` sul nome **in byte**, non secondo locale. Le directory sono emesse **prima** del loro contenuto.
5. **Root multiple**: `AddRoot` accetta path assoluti o relativi; il path nell'archivio è `filepath.Base(root)` + il percorso relativo, con separatore `/` sempre. Due root con lo stesso basename sono un errore esplicito (`hint`: usare `--strip-prefix` o rinominare) — questo evita collisioni silenziose.
6. **`Size` incoerente**: se il file cambia dimensione fra `Stat` e la lettura, in modalità strict è errore; altrimenti si trunca/pad e si registra in `Stats.Errors`. Mai produrre un tar corrotto.
7. **File sparsi**: in questa fase si scrivono **densi** (i buchi diventano zeri). L'ottimizzazione è fuori scope; annotare in `docs/FIDELITY.md`.
8. **`OneFileSystem`**: confronta `st.Dev` con quello della root.
9. **Path lunghi**: PAX gestisce nomi lunghi via record `path`. Nessun troncamento ammesso.
10. **Contesto**: `AddRoot` controlla `ctx.Err()` a ogni entry e a ogni 32 MiB copiati.

### Test richiesti
- round-trip in memoria: `Writer` → `Reader` → confronto campo per campo delle `Entry`;
- determinismo: archiviare due volte lo stesso albero produce **byte identici** (test cruciale per la dedup della fase 10; se fallisce, la fase 10 è impossibile);
- albero con 3 hardlink allo stesso inode: nel tar c'è **un** solo corpo dati;
- due root con lo stesso basename → errore con `Hint` non vuoto;
- confronto con `bsdtar`/GNU `tar`: `tar tvf` sull'archivio prodotto esce 0 e elenca lo stesso numero di voci (test gated sulla presenza del binario `tar`).

### Definition of Done
- [ ] tutti i test sopra verdi
- [ ] `GS-01.A`: `tar --xattrs --acls -tvf out.tar` di GNU tar non produce warning

---

## 01.5 Reader ed Extractor

**Agente: Sonnet**

### File: `pkg/archive/extract_unix.go` (`//go:build unix`)

### Ordine di applicazione obbligatorio

L'estrazione ha un ordine preciso; sbagliarlo produce perdita silenziosa di metadati.

```
per ogni entry:
  1. crea l'oggetto (file/dir/symlink/device/fifo/hardlink)
  2. scrivi il contenuto (solo file regolari)
  3. lchown(uid, gid)                  ← PRIMA di chmod: chown azzera setuid/setgid
  4. chmod(mode)                       ← non per i symlink (lchmod non esiste su Linux)
  5. setxattr(...)                     ← DOPO chown: security.capability viene azzerato da chown
  6. utimes(atime, mtime) con lutimes  ← ULTIMO fra i metadati della entry

alla fine di tutto:
  7. riapplica mode e timestamp a TUTTE le directory, dalla più profonda alla meno
     profonda (scrivere dentro una directory ne cambia mtime; una directory 0500
     non è scrivibile finché non hai finito di popolarla)
```

Questa sequenza va replicata come commento nel codice e come sezione in `docs/FIDELITY.md`.

### Altre prescrizioni

- **Sicurezza dei path**: prima di ogni creazione, verificare che il path risolto resti sotto `dest`. Usare `filepath.Clean` + controllo del prefisso, **e in aggiunta** rifiutare qualunque componente symlink già esistente che punti fuori (attacco "symlink swap"). Su Linux, quando disponibile, aprire le directory con `unix.Openat` + `O_NOFOLLOW` e operare con `*at()` relativi: è l'unica difesa robusta. Se ciò complica troppo, è ammesso il controllo per path, ma va dichiarato nel commento come limite noto e testato con una fixture d'attacco (`../../etc/passwd`, symlink verso `/tmp`).
- **Directory create al volo**: se una entry ha directory intermedie assenti (archivio manipolato), crearle con `0700` e registrarle per la riapplicazione finale.
- **Hardlink**: `os.Link` verso il path già estratto. Se il target non esiste ancora, è un archivio non valido → errore.
- **Device/FIFO**: `unix.Mknod`. Se `EPERM`, restituire `cli.Error{Kind: KindPermission, Hint: "run as root (sudo) to restore device nodes, or use --allow-degraded to skip them"}`.
- **`PreserveOwner` false**: salta il passo 3 e non è un errore.
- **Errori di xattr**: `system.posix_acl_*` e `security.*` falliscono con `EPERM` senza privilegi. In strict → errore con hint sui privilegi; altrimenti conteggiati.
- **Overwrite**: default `false`; se il target esiste, errore. Con `Overwrite=true`, rimuovere prima (`os.RemoveAll` solo per file/symlink; per directory, riusare).

### Test richiesti
- **Test di round-trip integrale**: `fixtures.Build` → `Writer` → `Extractor` → `fixtures.CompareTrees` con **zero differenze**. Questo è *il* test della fase.
  - variante non-root: `FeatBasic|FeatPerms|FeatSymlinks|FeatHardlinks|FeatXattrs|FeatNames|FeatTimes`
  - variante root (tag `root`): aggiunge `FeatACLs|FeatCaps|FeatDevices|FeatFifos|FeatOwnership`
- estrazione di un tar malevolo con `../../etc/passwd` → errore, nessuna scrittura fuori da `dest`;
- estrazione di un tar con symlink `evil -> /tmp` seguito da `evil/file` → errore;
- directory `0500` popolata correttamente e con permessi finali `0500`;
- file setuid: dopo l'estrazione il bit setuid è presente (verifica che chown preceda chmod);
- file con `security.capability`: dopo l'estrazione l'xattr è presente (verifica l'ordine chown→setxattr);
- estrazione due volte senza `Overwrite` → errore al secondo giro.

### Definition of Done
- [ ] round-trip a zero differenze in entrambe le varianti
- [ ] i 3 test di sicurezza sui path passano
- [ ] copertura `pkg/archive` ≥ 85 %

---

## 01.6 Backend Windows e specificità macOS

**Agente: Sonnet**

### File: `pkg/archive/meta_windows.go`, `pkg/archive/extract_windows.go` (`//go:build windows`)

- Usare `github.com/Microsoft/go-winio/backuptar`: `backuptar.FileInfoFromHeader` e `backuptar.WriteTarFileFromBackupStream` / `BasicInfoHeader`. Il security descriptor finisce nel record PAX `MSWINDOWS.rawsd`, gli alternate data stream in stream separati.
- Serve il privilegio `SeBackupPrivilege` / `SeRestorePrivilege`: abilitarli con `winio.EnableProcessPrivileges([]string{"SeBackupPrivilege","SeRestorePrivilege"})`. Se fallisce, errore `KindPermission` con hint "eseguire come amministratore".
- `Entry.UID/GID` su Windows restano `0`; l'identità sta nel security descriptor. `CompareOptions.IgnoreOwner` va impostato dai test Windows.
- Path: rifiutare i nomi riservati (`CON`, `PRN`, `AUX`, `NUL`, `COM1..9`, `LPT1..9`) e i caratteri `<>:"|?*` in estrazione, con errore chiaro (un tar Linux può contenerli legalmente).

### macOS
- `extract_unix.go` copre già macOS; aggiungere solo la gestione di `XATTR_NOFOLLOW` in `meta_darwin.go` se le firme differiscono.
- Le ACL NFSv4 di macOS **non** sono supportate: dichiararlo.

### Test richiesti
- CI `windows-latest`: round-trip con `FeatBasic|FeatPerms|FeatNames|FeatTimes` e `IgnoreOwner: true`, zero differenze;
- CI `macos-latest`: round-trip con `…|FeatXattrs|FeatSymlinks|FeatHardlinks`, zero differenze;
- test dei nomi riservati Windows.

### Definition of Done
- [ ] i job `test` su macOS e Windows passano
- [ ] `docs/FIDELITY.md` ha una tabella "cosa è preservato, per piattaforma"

---

## 01.7 Modalità strict, contabilità errori, preflight privilegi

**Agente: Sonnet**

### File: `pkg/archive/preflight.go`

```go
// Capability describes one privilege-dependent ability.
type Capability struct {
	Name      string // "read-all-files", "chown", "mknod", "set-security-xattr"
	Available bool
	Reason    string // why it is unavailable
	Remedy    string // exact command the user should run
}

// PreflightBackup inspects the environment and the given roots, and reports
// what will and will not be preserved.
func PreflightBackup(ctx context.Context, roots []string) ([]Capability, error)

// PreflightRestore reports whether a faithful restore is possible here.
func PreflightRestore(ctx context.Context, dest string) ([]Capability, error)
```

### Prescrizioni

- `read-all-files`: euid 0 **oppure** `CAP_DAC_READ_SEARCH` effettiva (leggere `/proc/self/status`, campo `CapEff`, bit 2). Rimedio: `sudo backimage …` **oppure** `sudo setcap cap_dac_read_search+ep $(which backimage)`.
- `chown`: euid 0 o `CAP_CHOWN` (bit 0). Rimedio: `sudo`.
- `mknod`: `CAP_MKNOD` (bit 27).
- `set-security-xattr`: `CAP_SETFCAP` (bit 31) per `security.capability`.
- **Scansione preventiva**: `PreflightBackup` fa un walk *leggero* (solo `Lstat` + tentativo di `Open` su un campione di al massimo 1000 file) e conta quanti sono illeggibili. Se > 0 e non si è root, la capability `read-all-files` è `Available: false` con `Reason` che riporta il conteggio e un esempio di path.
- **Comportamento in strict**: se una capability necessaria manca, il comando **fallisce prima di iniziare** con `KindPermission` e il `Remedy` come `Hint`. Nessun lavoro parziale.

### Test richiesti
- parsing di `CapEff` da una stringa `/proc/self/status` fittizia (iniettabile: la funzione di lettura è un campo sostituibile nei test);
- `PreflightBackup` su una directory con un file `0000` di un altro utente → `read-all-files` non disponibile con `Remedy` non vuoto (test gated: richiede due utenti, altrimenti simulare con `chmod 000` da root);
- strict=true su albero illeggibile → errore prima di scrivere il primo byte nel tar (verifica che il writer non abbia ricevuto nulla).

### Definition of Done
- [ ] i test passano
- [ ] `Remedy` non è mai vuoto quando `Available == false`

---

## 01.8 Test di round-trip completi e gating root

**Agente: Sonnet**

### File: `pkg/archive/roundtrip_test.go`, `pkg/archive/roundtrip_root_test.go` (`//go:build root`)

### Struttura obbligatoria

```go
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		feats fixtures.Feature
		opts  fixtures.CompareOptions
	}{ … }
	for _, tc := range cases { … }
}
```

### File: `test/e2e/phase_01.sh`

Script bash che:
1. costruisce `bin/backimage` (non ancora usato: la CLI di archiviazione arriva in fase 05 — lo script per ora esercita un piccolo binario di test `go run ./test/e2e/tools/archiveroundtrip`);
2. crea un albero in `$(mktemp -d)`;
3. esegue archiviazione ed estrazione;
4. confronta con `diff -r --no-dereference` **e** con `getfattr -d -R` e `getfacl -R` (se disponibili);
5. esce 1 alla prima differenza, stampando il diff.

Lo script deve funzionare anche senza root, saltando esplicitamente (`echo SKIP: …`) le parti che lo richiedono, **senza** fallire.

### Definition of Done
- [ ] `make e2e PHASE=01` esce 0 da utente normale
- [ ] `sudo make e2e PHASE=01` esce 0 ed esercita anche device, ownership e ACL
- [ ] `sudo -E go test -tags root ./pkg/archive/...` verde

---

## Gate di fase 01

| Gate | Comando | Criterio |
|---|---|---|
| G1–G6 | `make check` | exit 0 |
| G7 | `make cover PKG=./pkg/archive/...` | **≥ 85 %** |
| G8 | `make e2e PHASE=01` e `sudo make e2e PHASE=01` | exit 0 entrambi |
| **GS-01.1** | round-trip non-root | **zero** differenze |
| **GS-01.2** | round-trip root (tutte le feature) | **zero** differenze |
| **GS-01.3** | determinismo: due archiviazioni dello stesso albero | SHA-256 identico |
| **GS-01.4** | i 3 test di path traversal | tutti bloccati, nessuna scrittura fuori da `dest` |
| **GS-01.5** | `tar --xattrs --acls -tvf` GNU tar sull'output | exit 0, zero warning |
| **GS-01.6** | CI Windows e macOS | job `test` verdi |
| G10 | `docs/FIDELITY.md` esiste ed è completo | tabella per piattaforma + ordine di ripristino |
| G11 | revisione Opus | approvazione in `resume.md` |

**Deliverable documentali**: `docs/FIDELITY.md` (cosa è preservato per piattaforma, i due allentamenti ammessi — atime e ctime —, l'ordine di applicazione dei metadati, i privilegi richiesti e i comandi per ottenerli).

**Rischi noti**
- I record PAX con valori binari sono il punto più fragile: se il test del byte `\x00` fallisce, valutare la codifica base64 con chiave `SCHILY.xattr.<nome>` **più** un record `dev.backimage.xattr-b64` che elenca le chiavi codificate. Decisione da escalare a Opus, non da prendere in autonomia.
- `unshare -r` in CI dà un finto root: `mknod` fallisce comunque. Il job `test-root` deve usare `sudo` vero, non `unshare`.
