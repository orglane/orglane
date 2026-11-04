package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
)

// Snapshot layout (docs/specs/agent.md "Snapshots").
const (
	snapshotsDir     = "snapshots"
	snapshotMeta     = "snapshot.json"
	snapshotSpec     = "spec.json"
	snapshotRendered = "rendered"
	snapshotDB       = "db.dump"
	snapshotData     = "data"
	snapshotComplete = "COMPLETE"
)

// movedAsideRetention is how long data directories moved aside by a
// rollback or restore are kept (unless the next upgrade succeeds earlier).
const movedAsideRetention = 7 * 24 * time.Hour

// defaultStartTimeout bounds the start of a restored version whose package
// this binary does not embed.
const defaultStartTimeout = 5 * time.Minute

func (a *Agent) snapshotsPath(key string) string {
	return filepath.Join(a.instanceDir(key), snapshotsDir)
}

func (a *Agent) snapshotPath(key string, id uuid.UUID) string {
	return filepath.Join(a.snapshotsPath(key), id.String())
}

// backupPlan is what a snapshot of a spec contains. With the spec's package
// embedded it follows the manifest's backup section; otherwise (an older
// version that this release no longer embeds) it takes everything: the
// database if it exists and every data directory.
type backupPlan struct {
	database bool
	dataDirs []string
	images   map[string]string
}

func (a *Agent) backupPlan(ctx context.Context, key string, spec *agentapi.Spec) (backupPlan, error) {
	if pkg, ok := a.registry.Get(spec.Package); ok {
		m := pkg.Manifest
		return backupPlan{
			database: m.Database.Enabled && m.Backup.Database,
			dataDirs: slices.Clone(m.Backup.DataDirs),
			images:   maps.Clone(m.Images),
		}, nil
	}
	plan := backupPlan{images: map[string]string{}}
	entries, err := os.ReadDir(filepath.Join(a.instanceDir(key), dataDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return plan, err
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			plan.dataDirs = append(plan.dataDirs, e.Name())
		}
	}
	name := packages.DatabaseName(key)
	out, err := a.postgres().query(ctx, fmt.Sprintf("SELECT count(*) FROM pg_database WHERE datname = '%s';\n", name))
	if err != nil {
		return plan, err
	}
	plan.database = out == "1"
	return plan, nil
}

// snapshotSize estimates the space a snapshot of the plan needs: the
// database and the data directories, uncompressed.
func (a *Agent) snapshotSize(ctx context.Context, key string, plan backupPlan) (int64, error) {
	var total int64
	for _, d := range plan.dataDirs {
		n, err := dirSize(filepath.Join(a.instanceDir(key), dataDir, d))
		if err != nil {
			return 0, err
		}
		total += n
	}
	if plan.database {
		out, err := a.postgres().query(ctx, fmt.Sprintf("SELECT pg_database_size('%s');\n", packages.DatabaseName(key)))
		if err != nil {
			return 0, err
		}
		n, err := strconv.ParseInt(out, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("database size %q: %w", out, err)
		}
		total += n
	}
	return total, nil
}

// checkDisk is the snapshot preflight: free space under instances/ must be
// at least 1.2 × the snapshot's size + 1 GiB.
func (a *Agent) checkDisk(ctx context.Context, key string, plan backupPlan) error {
	size, err := a.snapshotSize(ctx, key, plan)
	if err != nil {
		return failed(agentapi.CodeDatabaseFailed, err)
	}
	need := uint64(size)*6/5 + 1<<30 //nolint:gosec // sizes are not negative
	if free := a.diskFree(a.instancesDir); free < need {
		return failed(agentapi.CodeInsufficientDisk, fmt.Errorf("%d bytes free under instances/, %d needed", free, need))
	}
	return nil
}

// takeSnapshot snapshots an instance whose services are stopped: the spec,
// the rendered files, the database dump and the data directories, then
// snapshot.json and, written and synced last, COMPLETE.
func (a *Agent) takeSnapshot(ctx context.Context, key string, spec *agentapi.Spec, plan backupPlan, reason string, id uuid.UUID) error {
	if err := os.MkdirAll(a.snapshotsPath(key), 0o700); err != nil {
		return err
	}
	dir := a.snapshotPath(key, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	snap := &agentapi.Snapshot{
		Format: agentapi.SnapshotFormat, ID: id, CreatedAt: a.now().UTC(), Reason: reason,
		Package: spec.Package, Generation: spec.Generation, Images: plan.images, DataDirs: []agentapi.SnapshotDataDir{},
	}
	if err := writeSpec(filepath.Join(dir, snapshotSpec), spec); err != nil {
		return err
	}
	hash, err := copyRendered(a.instanceDir(key), filepath.Join(dir, snapshotRendered))
	if err != nil {
		return fmt.Errorf("copy rendered files: %w", err)
	}
	snap.RenderedSHA256 = hash
	if plan.database {
		f, err := writeSnapshotFile(filepath.Join(dir, snapshotDB), func(w io.Writer) error {
			return a.postgres().dump(ctx, packages.DatabaseName(key), w)
		})
		if err != nil {
			return fmt.Errorf("dump the database: %w", err)
		}
		f.File = snapshotDB
		snap.Database = &f
	}
	if len(plan.dataDirs) > 0 {
		if err := os.Mkdir(filepath.Join(dir, snapshotData), 0o700); err != nil {
			return err
		}
	}
	for _, name := range plan.dataDirs {
		src := filepath.Join(a.instanceDir(key), dataDir, name)
		if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
			continue // never created: nothing to restore
		}
		rel := snapshotData + "/" + name + ".tar.gz"
		f, err := writeSnapshotFile(filepath.Join(dir, filepath.FromSlash(rel)), func(w io.Writer) error {
			return archiveDir(src, w)
		})
		if err != nil {
			return fmt.Errorf("archive data directory %s: %w", name, err)
		}
		f.File = rel
		snap.DataDirs = append(snap.DataDirs, agentapi.SnapshotDataDir{Name: name, SnapshotFile: f})
	}
	return completeSnapshot(dir, snap, syncDir)
}

// completeSnapshot persists all parent entries before publishing COMPLETE.
// The injected directory sync lets tests verify writeback failure handling.
func completeSnapshot(dir string, snap *agentapi.Snapshot, sync func(string) error) error {
	data := filepath.Join(dir, snapshotData)
	if _, err := os.Stat(data); err == nil {
		if err := sync(data); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := sync(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := sync(filepath.Dir(filepath.Dir(dir))); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, snapshotMeta), snap); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, snapshotComplete), []byte(snap.ID.String()+"\n"), 0o600)
}

// writeSnapshotFile writes a file through fn, syncs it and returns its size
// and SHA-256.
func writeSnapshotFile(path string, fn func(io.Writer) error) (agentapi.SnapshotFile, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // agent-owned path
	if err != nil {
		return agentapi.SnapshotFile{}, err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	err = fn(cw)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return agentapi.SnapshotFile{}, err
	}
	return agentapi.SnapshotFile{Bytes: cw.n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// renderedFiles are the instance's rendered files a snapshot keeps, with
// their modes; files/ is copied as a tree.
var renderedFiles = []struct {
	name string
	mode fs.FileMode
}{{composeFile, 0o644}, {envFile, 0o600}, {configuredFile, 0o600}}

// copyRendered copies compose.yaml, .env, configured.json (if present) and
// files/ of an instance into dst and returns a hash over them.
func copyRendered(instanceDir, dst string) (string, error) {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return "", err
	}
	for _, f := range renderedFiles {
		data, err := os.ReadFile(filepath.Join(instanceDir, f.name)) //nolint:gosec // agent-owned path
		if errors.Is(err, fs.ErrNotExist) && f.name == configuredFile {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := writeFileAtomic(filepath.Join(dst, f.name), data, f.mode); err != nil {
			return "", err
		}
	}
	files, err := readTree(filepath.Join(instanceDir, packages.FilesDir))
	if err != nil {
		return "", err
	}
	for _, name := range sortedKeys(files) {
		f := files[name]
		p := filepath.Join(dst, packages.FilesDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", err
		}
		if err := writeFileAtomic(p, f.Data, f.Mode); err != nil {
			return "", err
		}
	}
	if err := syncTreeDirs(dst); err != nil {
		return "", err
	}
	r, configured, err := readRenderedProject(dst)
	if err != nil {
		return "", err
	}
	return renderedDigest(r, configured), nil
}

// renderedDigest uses the same encoding for snapshot creation and verification.
func renderedDigest(r *packages.Rendered, configured []byte) string {
	h := sha256.New()
	add := func(name string, mode fs.FileMode, data []byte) {
		_, _ = fmt.Fprintf(h, "%s\x00%o\x00%d\x00", name, mode, len(data))
		_, _ = h.Write(data)
	}
	add(composeFile, 0o644, r.Compose)
	add(envFile, 0o600, r.Env)
	if configured != nil {
		add(configuredFile, 0o600, configured)
	}
	for _, name := range sortedKeys(r.Files) {
		f := r.Files[name]
		add(packages.FilesDir+"/"+name, f.Mode, f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// syncTreeDirs persists directory entries bottom-up without following symlinks.
// File contents must already have been synced by their writer.
func syncTreeDirs(dir string) error {
	var dirs []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	}); err != nil {
		return err
	}
	slices.Reverse(dirs)
	for _, d := range dirs {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

// readTree reads the regular files under dir by slash-separated path; a
// missing dir is empty.
func readTree(dir string) (map[string]packages.File, error) {
	out := map[string]packages.File{}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		out[name] = packages.File{Data: data, Mode: fi.Mode().Perm()}
		return nil
	})
	return out, err
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// findSnapshot returns the ID of an instance's snapshot directory that
// equals want, as read from the directory.
func (a *Agent) findSnapshot(key string, want uuid.UUID) (uuid.UUID, bool) {
	entries, err := os.ReadDir(a.snapshotsPath(key))
	if err != nil {
		return uuid.Nil, false
	}
	for _, e := range entries {
		if id, err := uuid.Parse(e.Name()); err == nil && e.IsDir() && id == want && id.String() == e.Name() {
			return id, true
		}
	}
	return uuid.Nil, false
}

// loadSnapshot reads a complete snapshot and its spec.
func (a *Agent) loadSnapshot(key string, id uuid.UUID) (*agentapi.Snapshot, *agentapi.Spec, error) {
	dir := a.snapshotPath(key, id)
	if _, err := os.Stat(filepath.Join(dir, snapshotComplete)); err != nil {
		return nil, nil, fmt.Errorf("snapshot %s is not complete: %w", id, err)
	}
	var snap agentapi.Snapshot
	if _, err := readJSON(filepath.Join(dir, snapshotMeta), &snap); err != nil {
		return nil, nil, err
	}
	if snap.ID != id || snap.Format != agentapi.SnapshotFormat {
		return nil, nil, fmt.Errorf("snapshot %s: unexpected metadata", id)
	}
	spec, err := readSpec(filepath.Join(dir, snapshotSpec))
	if err != nil {
		return nil, nil, err
	}
	if spec == nil {
		return nil, nil, fmt.Errorf("snapshot %s has no spec", id)
	}
	return &snap, spec, nil
}

// listSnapshots returns the complete snapshots of an instance, newest first.
func (a *Agent) listSnapshots(key string) ([]agentapi.Snapshot, error) {
	entries, err := os.ReadDir(a.snapshotsPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return []agentapi.Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []agentapi.Snapshot{}
	for _, e := range entries {
		id, err := uuid.Parse(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(a.snapshotPath(key, id), snapshotComplete)); err != nil {
			continue
		}
		snap, _, err := a.loadSnapshot(key, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *snap)
	}
	// UUIDv7 IDs sort by creation time.
	slices.SortFunc(out, func(x, y agentapi.Snapshot) int { return strings.Compare(y.ID.String(), x.ID.String()) })
	return out, nil
}

// removePartialSnapshots deletes snapshot directories without COMPLETE
// (docs/specs/agent.md "Snapshots"), except keep.
func (a *Agent) removePartialSnapshots(key string, keep uuid.UUID) error {
	entries, err := os.ReadDir(a.snapshotsPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() == keep.String() && keep != uuid.Nil {
			continue
		}
		p := filepath.Join(a.snapshotsPath(key), e.Name())
		if _, err := os.Stat(filepath.Join(p, snapshotComplete)); errors.Is(err, fs.ErrNotExist) {
			a.log.Warn("removing a partial snapshot", "instance", key, "snapshot", e.Name())
			errs = append(errs, os.RemoveAll(p))
		}
	}
	return errors.Join(errs...)
}

// pruneSnapshots keeps the newest agent.snapshot_retention upgrade
// snapshots; manual snapshots stay (docs/specs/agent.md "Snapshots").
func (a *Agent) pruneSnapshots(key string) error {
	list, err := a.listSnapshots(key)
	if err != nil {
		return err
	}
	kept := 0
	var errs []error
	for _, s := range list {
		if s.Reason != agentapi.SnapshotReasonUpgrade {
			continue
		}
		kept++
		if kept > max(a.cfg.SnapshotRetention, 1) {
			errs = append(errs, os.RemoveAll(a.snapshotPath(key, s.ID)))
		}
	}
	return errors.Join(errs...)
}

// verifySnapshot checks the sizes and checksums of a snapshot's dump and
// archives before anything is restored from it.
func (a *Agent) verifySnapshot(key string, snap *agentapi.Snapshot) error {
	dir := a.snapshotPath(key, snap.ID)
	files := []agentapi.SnapshotFile{}
	if snap.Database != nil {
		files = append(files, *snap.Database)
	}
	for _, d := range snap.DataDirs {
		files = append(files, d.SnapshotFile)
	}
	for _, f := range files {
		if !fs.ValidPath(f.File) {
			return fmt.Errorf("snapshot %s: invalid file name %q", snap.ID, f.File)
		}
		fh, err := os.Open(filepath.Join(dir, filepath.FromSlash(f.File))) //nolint:gosec // validated, agent-owned path
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, fh)
		_ = fh.Close()
		if err != nil {
			return err
		}
		if n != f.Bytes || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return fmt.Errorf("snapshot %s: %s does not match its checksum", snap.ID, f.File)
		}
	}
	return nil
}

// movedAside returns the name a data directory is moved aside to by the
// rollback or restore operation op.
func movedAside(name string, op uuid.UUID) string { return "." + name + ".failed-" + op.String() }

// restoreDataDirs restores the snapshot's data directories (rollback phase
// 1): each current directory is moved aside once (data/.<name>.failed-<op>),
// then the archive is extracted into a temporary directory that replaces
// it. Repeating it after an interruption extracts again and never moves the
// moved-aside data a second time.
func (a *Agent) restoreDataDirs(key string, snap *agentapi.Snapshot, op uuid.UUID) error {
	root := filepath.Join(a.instanceDir(key), dataDir)
	if err := os.MkdirAll(root, 0o755); err != nil { //nolint:gosec // like ensureDataDirs
		return err
	}
	chown := os.Geteuid() == 0
	for _, d := range snap.DataDirs {
		cur := filepath.Join(root, d.Name)
		aside := filepath.Join(root, movedAside(d.Name, op))
		tmp := filepath.Join(root, "."+d.Name+".restore-"+op.String())
		if _, err := os.Lstat(aside); errors.Is(err, fs.ErrNotExist) {
			if _, err := os.Lstat(cur); err == nil {
				if err := os.Rename(cur, aside); err != nil {
					return err
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		} else if err != nil {
			return err
		} else if err := os.RemoveAll(cur); err != nil {
			// Moved aside before: cur is a partial earlier restore.
			return err
		}
		if err := os.RemoveAll(tmp); err != nil {
			return err
		}
		if err := os.Mkdir(tmp, 0o700); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(a.snapshotPath(key, snap.ID), filepath.FromSlash(d.File))) //nolint:gosec // validated by verifySnapshot
		if err != nil {
			return err
		}
		err = extractArchive(f, tmp, chown)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("extract %s: %w", d.Name, err)
		}
		if err := os.Rename(tmp, cur); err != nil {
			return err
		}
	}
	if err := syncDir(root); err != nil {
		return err
	}
	return syncDir(a.instanceDir(key))
}

// restoreDatabase replaces the instance's database with the snapshot's
// dump (rollback phase 1): the role is ensured with the snapshot's password,
// the database is dropped (closing its connections) and created empty, and
// pg_restore restores it with every object owned by the role. Safe to
// repeat.
func (a *Agent) restoreDatabase(ctx context.Context, key string, snap *agentapi.Snapshot, password string) error {
	if !packages.ValidSecret(password) {
		return errors.New("invalid database password")
	}
	name := packages.DatabaseName(key)
	sql := fmt.Sprintf(`SELECT format('CREATE ROLE %%I LOGIN', '%[1]s') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s')\gexec
ALTER ROLE %[1]s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE CONNECTION LIMIT 20 PASSWORD %[2]s;
DROP DATABASE IF EXISTS %[1]s WITH (FORCE);
CREATE DATABASE %[1]s OWNER %[1]s ENCODING 'UTF8' TEMPLATE template0;
REVOKE ALL ON DATABASE %[1]s FROM PUBLIC;
`, name, sqlString(password))
	if err := a.postgres().exec(ctx, sql, []string{password}); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(a.snapshotPath(key, snap.ID), filepath.FromSlash(snap.Database.File))) //nolint:gosec // validated by verifySnapshot
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return a.postgres().restore(ctx, name, name, f)
}

// snapshotRenderedProject reads a snapshot's rendered files as a project to
// write with writeRendered, fenced if asked.
func (a *Agent) snapshotRenderedProject(key string, id uuid.UUID, fenced bool) (*packages.Rendered, []byte, error) {
	snap, _, err := a.loadSnapshot(key, id)
	if err != nil {
		return nil, nil, err
	}
	r, configured, err := readRenderedProject(filepath.Join(a.snapshotPath(key, id), snapshotRendered))
	if err != nil {
		return nil, nil, err
	}
	// Pre-alpha snapshots without a digest cannot establish integrity either.
	if snap.RenderedSHA256 == "" || renderedDigest(r, configured) != snap.RenderedSHA256 {
		return nil, nil, fmt.Errorf("snapshot %s: rendered project does not match its checksum", id)
	}
	if fenced {
		if r.Compose, err = fenceCompose(r.Compose); err != nil {
			return nil, nil, err
		}
	}
	return r, configured, nil
}

func readRenderedProject(dir string) (*packages.Rendered, []byte, error) {
	compose, err := os.ReadFile(filepath.Join(dir, composeFile)) //nolint:gosec // agent-owned path
	if err != nil {
		return nil, nil, err
	}
	env, err := os.ReadFile(filepath.Join(dir, envFile)) //nolint:gosec // agent-owned path
	if err != nil {
		return nil, nil, err
	}
	files, err := readTree(filepath.Join(dir, packages.FilesDir))
	if err != nil {
		return nil, nil, err
	}
	configured, err := os.ReadFile(filepath.Join(dir, configuredFile)) //nolint:gosec // agent-owned path
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	return &packages.Rendered{Compose: compose, Env: env, Files: files}, configured, nil
}

// writeSnapshotRendered writes a snapshot's rendered files into the
// instance directory, fenced or as they were, and its configure record.
// A fenced project is normalized and checked first: no published ports,
// only fenced aliases.
func (a *Agent) writeSnapshotRendered(ctx context.Context, key string, id uuid.UUID, fenced bool, c compose) error {
	r, configured, err := a.snapshotRenderedProject(key, id, fenced)
	if err != nil {
		return err
	}
	if fenced {
		original, _, err := a.snapshotRenderedProject(key, id, false)
		if err != nil {
			return err
		}
		aliases, err := composeAliases(original.Compose)
		if err != nil {
			return err
		}
		if err := a.stage(c, r, func(file, env string) error {
			normalized, err := c.config(ctx, file, env)
			if err != nil {
				return err
			}
			return checkFenced(normalized, aliases...)
		}); err != nil {
			return err
		}
	}
	rc := a.renderContext(key)
	if err := writeRendered(rc, r); err != nil {
		return err
	}
	p := filepath.Join(a.instanceDir(key), configuredFile)
	if configured == nil {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeFileAtomic(p, configured, 0o600)
}

// stage writes a project to the staging directory and calls fn with its
// compose and env files; the staging directory is removed afterwards.
func (a *Agent) stage(c compose, r *packages.Rendered, fn func(file, env string) error) error {
	staging := filepath.Join(c.dir, stagingDir)
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	file, env := filepath.Join(staging, composeFile), filepath.Join(staging, envFile)
	if err := os.WriteFile(file, r.Compose, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(env, r.Env, 0o600); err != nil {
		return err
	}
	return fn(file, env)
}

// cleanMovedAside deletes data directories moved aside by earlier
// rollbacks and restores: all of them (after a successful upgrade), or
// those older than movedAsideRetention. Operation IDs are UUIDv7, so the
// name tells the time.
func (a *Agent) cleanMovedAside(key string, all bool) error {
	root := filepath.Join(a.instanceDir(key), dataDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		i := strings.LastIndex(name, ".failed-")
		if !strings.HasPrefix(name, ".") || i < 0 {
			continue
		}
		id, err := uuid.Parse(name[i+len(".failed-"):])
		if err != nil {
			continue
		}
		sec, nsec := id.Time().UnixTime()
		if all || a.now().Sub(time.Unix(sec, nsec)) > movedAsideRetention {
			errs = append(errs, os.RemoveAll(filepath.Join(root, name)))
		}
	}
	return errors.Join(errs...)
}
