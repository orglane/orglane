package agent

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// archiveDir writes the tree under dir as a gzip-compressed tar to w
// (docs/specs/agent.md "Snapshots"): owners, modes, modification times and
// symlinks are kept; sockets, devices and pipes are skipped. dir is opened
// as an os.Root, so a symlink that a service placed in its data directory
// is archived as a link and never followed.
func archiveDir(dir string, w io.Writer) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info() // lstat
		if err != nil {
			return err
		}
		link := ""
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			if link, err = root.Readlink(name); err != nil {
				return err
			}
		case fi.IsDir(), fi.Mode().IsRegular():
		default:
			return nil // sockets, devices, named pipes
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		// The root itself is "./": its mode and owner are restored too.
		hdr.Name = name
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", "" // numeric owners only
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, f, hdr.Size)
		return errors.Join(err, f.Close())
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// extractArchive extracts an archive written by archiveDir into dir, which
// must exist and be empty. Every path stays inside dir (os.Root): entries
// that would leave it, through ".." or a symlink, fail the extraction.
// Owners are applied only when chown is set (the agent runs as root).
func extractArchive(r io.Reader, dir string, chown bool) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	tr := tar.NewReader(gz)

	type dirMeta struct {
		name  string
		mode  fs.FileMode
		mtime time.Time
	}
	var dirs []dirMeta
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if !fs.ValidPath(name) || (name == "." && hdr.Typeflag != tar.TypeDir) {
			return fmt.Errorf("archive: invalid entry %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if name == "." {
				dirs = append(dirs, dirMeta{name: name, mode: unixMode(hdr.Mode), mtime: hdr.ModTime})
				break
			}
			// Writable for now; the final mode is applied at the end, so a
			// read-only directory does not block its own entries.
			if err := root.Mkdir(name, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{name: name, mode: unixMode(hdr.Mode), mtime: hdr.ModTime})
		case tar.TypeReg:
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, tr, hdr.Size)
			if err = errors.Join(err, f.Close()); err != nil {
				return err
			}
			if err := root.Chmod(name, unixMode(hdr.Mode)); err != nil {
				return err
			}
			if err := root.Chtimes(name, hdr.ModTime, hdr.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive: unsupported entry type %q for %s", hdr.Typeflag, hdr.Name)
		}
		if chown {
			if err := root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
				return err
			}
		}
	}
	// Deepest first, so a parent's mode and time are set after its entries
	// (the root, the first entry, comes last).
	slices.Reverse(dirs)
	for _, d := range dirs {
		if err := root.Chmod(d.name, d.mode); err != nil {
			return err
		}
		if err := root.Chtimes(d.name, d.mtime, d.mtime); err != nil {
			return err
		}
	}
	return nil
}

// unixMode converts tar mode bits to a FileMode with setuid, setgid and
// sticky bits.
func unixMode(m int64) fs.FileMode {
	mode := fs.FileMode(m & 0o777) //nolint:gosec // masked
	// The tar header bits c_ISUID, c_ISGID and c_ISVTX.
	if m&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// dirSize returns the total size of the regular files under dir, without
// following symlinks; 0 if dir does not exist.
func dirSize(dir string) (int64, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	return total, err
}
