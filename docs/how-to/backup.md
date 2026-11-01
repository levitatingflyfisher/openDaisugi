# Back up and restore your history

Two commands save your work to one file. Two commands put it back on a new
machine or a fresh install.

```
daisugi backup [FILE]      default: ./daisugi-backup-DATE.tgz
daisugi restore FILE [--force]
coppice backup [FILE]      default: ./coppice-backup-DATE.tgz
coppice restore FILE [--force]
```

DATE is the UTC date. Both ports of each tool (Go and Rust) write and read the
same file.

## What is in a backup

`daisugi backup` holds, from the data home (`OPENDAISUGI_HOME` or `--data-dir`
if you set one; else `~/.opendaisugi`; else `$XDG_DATA_HOME/opendaisugi` when
`~/.opendaisugi` does not exist):

- `config.yaml`, which also holds your agent profiles
- `pathways.db`
- `journal/`, the traces and the index
- `warden/envelopes/` and `warden/audit/`
- your conversation memory: every Pando R2 store in `pando/` (see below)

The two SQLite files, `pathways.db` and `journal/index.db`, go in as a clean
copy that SQLite makes (`VACUUM INTO`). A running session cannot tear it, and
the `-wal` and `-shm` files are not needed.

`coppice backup` holds:

- your conversation memory: every Pando R2 store (see below). With the default
  data dir, the stores come from `pando/` in the data home. With `--data-dir X`,
  they come from `X/pando`
- a chat that `daisugi pando migrate` has not moved yet (its `main`, `tree`,
  `view.json` and `import.json`)
- `layout.json` and `recent-dirs.json` from the coppice data dir
- `coppice.toml` and `pools.toml` from the XDG config dir (`~/.config/coppice/`).
  They always come from there, even when you pass `--data-dir`

Both tools hold the same parts of each store, from one list that the pando
code keeps: `main/` (the log), `tree/`, `view.json`, `import.json` (the import
queue and its `--map`), and `children/` (each worker's transcript store). A
store's `lock` is never in a backup. Every store is in it, not only `work` and
`home`. A restore puts back each store the archive holds, also one that the
target does not have yet. An archive that `coppice backup` made before the move
holds the chat as `foreman/`; a restore puts it into the `work` store.

Each file is on a list of paths that a backup may hold. A file that is not on
the list is not in the backup. Keys, tokens, credentials, sockets and locks are
never on the list, and a restore refuses any file that is not on it, even if
someone changed the archive by hand. Inside the listed folders, a file whose
name looks like a secret or a lock is also left out. The backup says how many
it left out.

No key, token or credential files go in. Your chat and logs do, so keep the
file as private as the data. The file has mode 0600. A key that you pasted into
a chat, or that a command line held, is in that text.

A backup does not follow a symbolic link at the top of a listed folder. If
`~/.opendaisugi/journal` is a link, the backup stops and names the path. Back
up the real folder.

## Make a backup

```
daisugi backup
coppice backup ~/backups/coppice-before-upgrade.tgz
```

A backup never overwrites a file. Name a new one. For an exact copy, stop the
server first with `coppice server stop`. The backup reads each file once. It
compares the size and the modification time before and after, and stops with a
message if either changed. Run it again, or stop the tool first.

`daisugi backup` can run while a session is live, because the SQLite files are
copied through SQLite. The journal and audit files can still change while it
runs. If one does, the backup stops with the message above.

## Restore

```
daisugi restore daisugi-backup-2026-10-24.tgz
coppice server stop
coppice restore coppice-backup-2026-10-24.tgz
```

Restore reads the file once. It writes each file into a staging folder
(`.restore-TIME`) next to its target, and checks the name, the type and the
checksum as it writes. If anything is wrong, it removes the staging folder and
changes nothing. Only when every file is good does it swap the files in, with
renames.

An archive that lists no files is refused.

A target that already holds data is refused. Add `--force`: restore then moves
each old file or directory aside, to `NAME.broken-TIME`, and puts the backup
in its place. It moves aside every path a backup can hold, including paths that
this backup does not hold. Nothing is deleted. Remove the `.broken-` copies
yourself when you are sure.

If a rename fails during the swap, restore undoes the renames it did, in
reverse order. Your old files are back in place. The restored files stay in the
staging folder, and the error line names it. If an undo step fails too, the
error names the paths that could not be moved back. They are still under their
`.broken-TIME` names.

`daisugi restore` refuses while a warden answers on `warden/warden.sock` in the data
home. A live warden writes the journal and the audit on every call. Stop agent
sessions, or run `daisugi warden disarm`, first.

The restore prints each directory it wrote into. For coppice, the config files
go to the XDG config dir even when you pass `--data-dir`. `backup` and
`restore` work on local files only. `coppice --remote` refuses them.

`coppice restore` needs the server stopped, because the server holds the
layout and steward files open. It says so in one line if the server runs.
After a restore, run `coppice server start`.

A store that Pando moved aside after damage is kept on disk as `<store>.broken-TIME`.
The backup does not hold these directories. Copy them out by hand if you want them.
