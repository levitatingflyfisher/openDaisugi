package cli

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3" // the SQLite driver, the amalgamation linked in

	"daisugi-verify/internal/backup"
	"daisugi-verify/internal/sqlpath"
)

// vacuumInto writes a consistent copy of the SQLite file src at dst, while
// another process may write src.
func vacuumInto(src, dst string) error {
	dsn, err := sqlpath.DSN(src, "_busy_timeout=5000")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("it is not a SQLite file that reads: %v", err)
	}
	return nil
}

// backupSpec is what `daisugi backup` holds: history and settings, never
// keys, tokens, sockets or locks. It is an allow-list. A new file in the
// data home stays out until it is added here.
func backupSpec(dataDir string) backup.Spec {
	return backup.Spec{
		Tool:    "daisugi",
		Version: version(),
		Roots:   map[string]string{"data": dataDir},
		Units: []backup.Unit{
			{Area: "data", Rel: "config.yaml"},
			{Area: "data", Rel: "pathways.db"},
			{Area: "data", Rel: "journal"},
			{Area: "data", Rel: "gate/envelopes"},
			{Area: "data", Rel: "gate/audit"},
		},
		// The two SQLite files are copied with VACUUM INTO, so a live
		// session cannot tear them and no -wal file is needed.
		Snapshots: []string{"data/pathways.db", "data/journal/index.db"},
		Snap:      vacuumInto,
	}
}

func (e *Env) backupCmd(args []string) error {
	const cmd = "backup"
	opts := []opt{dataDirOpt}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " [FILE]", "Write your journal, pathways, envelopes and config to one .tgz (default ./daisugi-backup-DATE.tgz). No key, token or credential files go in. Your chat and logs do, so keep the file as private as the data.", opts)
	}
	now := time.Now()
	out := "daisugi-backup-" + now.UTC().Format("2006-01-02") + ".tgz"
	if len(p.args) == 1 {
		out = p.args[0]
	}
	r, err := backup.Create(backupSpec(e.dataDir(p)), out, now)
	if err != nil {
		return e.fail(cmd, err)
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	if r.Left > 0 {
		e.out("Left out %d files whose names look like secrets or locks.\n", r.Left)
	}
	e.out("Backed up %d files (%d bytes) from %s to %s.\n", r.Files, r.Bytes, e.dataDir(p), out)
	return nil
}

func (e *Env) restoreCmd(args []string) error {
	const cmd = "restore"
	opts := []opt{
		dataDirOpt,
		{names: []string{"--force"}, help: "Move the data now there aside (.broken-TIME) and restore."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, " FILE", "Put a daisugi backup back. It refuses to overwrite data unless --force; then the old data is moved aside, never deleted.", opts)
	}
	if len(p.args) != 1 {
		return e.usage(cmd, &usageError{"Missing argument 'FILE'."})
	}
	dir := e.dataDir(p)
	// A running gate writes the journal and the audit on every call. Do not
	// move those files from under it.
	for _, sock := range []string{filepath.Join(dir, "gate", "gate.sock"), filepath.Join(dir, "gate.sock")} {
		if probeGate(sock) == gateLive {
			return e.fail(cmd, fmt.Errorf("a gate answers on %s. Stop agent sessions or run `daisugi gate disarm` first", sock))
		}
	}
	r, err := backup.Restore(backupSpec(dir), p.args[0], p.flag("--force"), time.Now())
	for _, a := range r.Aside {
		e.out("Moved aside: %s\n", a)
	}
	if err != nil {
		return e.fail(cmd, err)
	}
	e.out("Restored %d files (%d bytes) into %s.\n", r.Files, r.Bytes, dir)
	return nil
}
