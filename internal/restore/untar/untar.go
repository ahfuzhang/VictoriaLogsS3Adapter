// Package untar safely extracts VictoriaLogs partition backups (tar archives).
package untar

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Directory names of a VictoriaLogs partition.
const (
	IndexdbDir = "indexdb"
	DatadbDir  = "datadb"
)

// Stats describes an extracted archive.
type Stats struct {
	Files     int
	Dirs      int
	HardLinks int
	Skipped   int
	// DataBytes is the total size of extracted regular files.
	DataBytes int64
}

const copyBufSize = 1 << 20

// Extract extracts the tar stream r into the existing directory dir.
//
// Only directories, regular files and hard links to previously extracted
// regular files are created. Symlinks, devices, FIFOs and other entry types are
// skipped. Entries with absolute names or names escaping dir via ".." make
// Extract fail. File modes from the archive are ignored: directories are created
// with 0755 and files with 0644 permissions.
//
// Every extracted file is fsynced, and all the directories under dir (including
// dir itself) are fsynced after a successful extraction, so the extracted data
// survives a crash once it is moved into place.
//
// ctx cancellation is checked between entries; the caller should also make r
// return an error on cancellation for long entries.
func Extract(ctx context.Context, r io.Reader, dir string) (Stats, error) {
	var st Stats
	tr := tar.NewReader(r)
	buf := make([]byte, copyBufSize)
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			if err := SyncDirs(dir); err != nil {
				return st, fmt.Errorf("cannot sync extracted directories: %w", err)
			}
			return st, nil
		}
		if err != nil {
			return st, fmt.Errorf("cannot read tar entry header: %w", err)
		}
		rel, err := SanitizeName(hdr.Name)
		if err != nil {
			return st, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if rel == "" {
				continue
			}
			if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(rel)), 0o755); err != nil {
				return st, fmt.Errorf("cannot create directory for tar entry %q: %w", hdr.Name, err)
			}
			st.Dirs++
		case tar.TypeReg, tar.TypeGNUSparse:
			if rel == "" {
				return st, fmt.Errorf("unexpected regular file tar entry %q pointing to the archive root", hdr.Name)
			}
			n, err := extractFile(tr, dir, rel, buf)
			if err != nil {
				return st, fmt.Errorf("cannot extract tar entry %q: %w", hdr.Name, err)
			}
			st.Files++
			st.DataBytes += n
		case tar.TypeLink:
			if rel == "" {
				return st, fmt.Errorf("unexpected hard link tar entry %q pointing to the archive root", hdr.Name)
			}
			if err := extractHardLink(dir, rel, hdr.Linkname); err != nil {
				return st, fmt.Errorf("cannot extract hard link tar entry %q -> %q: %w", hdr.Name, hdr.Linkname, err)
			}
			st.HardLinks++
		case tar.TypeXGlobalHeader:
			// PAX global header; it carries no file data.
			continue
		default:
			slog.Warn("skipping unsupported tar entry", "name", hdr.Name, "type", string(hdr.Typeflag))
			st.Skipped++
		}
	}
}

// SanitizeName validates the tar entry name and returns it as a cleaned relative
// slash-separated path. An empty result means the archive root (e.g. "./").
func SanitizeName(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty tar entry name")
	}
	if strings.IndexByte(name, 0) >= 0 {
		return "", fmt.Errorf("tar entry name %q contains NUL byte", name)
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("refusing tar entry with absolute name %q", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("refusing tar entry %q escaping the target directory", name)
	}
	return clean, nil
}

func extractFile(r io.Reader, dir, rel string, buf []byte) (int64, error) {
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	// Remove a previous entry with the same name (tar archives may contain duplicates; the last one wins).
	// This also prevents writing through a hard link created by a previous entry.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.CopyBuffer(f, r, buf)
	if err != nil {
		_ = f.Close()
		return n, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return n, err
	}
	if err := f.Close(); err != nil {
		return n, err
	}
	return n, nil
}

func extractHardLink(dir, rel, linkname string) error {
	linkRel, err := SanitizeName(linkname)
	if err != nil {
		return err
	}
	if linkRel == "" {
		return fmt.Errorf("hard link target %q points to the archive root", linkname)
	}
	src := filepath.Join(dir, filepath.FromSlash(linkRel))
	fi, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("hard link target must be extracted before the link: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("hard link target %q is not a regular file", linkname)
	}
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Link(src, target)
}

// SyncDirs fsyncs dir and all the directories under it, so their entries are persisted.
func SyncDirs(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		return SyncDir(p)
	})
}

// SyncDir fsyncs the directory p.
func SyncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot sync directory %q: %w", p, err)
	}
	return f.Close()
}

// FindDataRoot returns the directory under dir that holds the VictoriaLogs
// partition data, i.e. the directory directly containing the indexdb and datadb
// directories.
//
// It is either dir itself (an archive of the partition contents) or a single
// subdirectory of dir (an archive of a snapshot directory such as
// ./20261104091211-18C876EFEB03599B). Other top-level entries without indexdb,
// such as macOS AppleDouble files, are ignored.
func FindDataRoot(dir string) (string, error) {
	if ok, err := isPartitionDir(dir); err != nil {
		return "", err
	} else if ok {
		return dir, nil
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var roots []string
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		p := filepath.Join(dir, de.Name())
		ok, err := isPartitionDir(p)
		if err != nil {
			return "", err
		}
		if ok {
			roots = append(roots, p)
		}
	}
	switch len(roots) {
	case 0:
		return "", fmt.Errorf("the archive contains no directory with %q and %q subdirectories at the top level", IndexdbDir, DatadbDir)
	case 1:
		return roots[0], nil
	default:
		return "", fmt.Errorf("the archive contains %d top-level directories with partition data; expecting exactly one", len(roots))
	}
}

// isPartitionDir reports whether dir contains indexdb and datadb directories.
// It returns an error if only one of them exists, since VictoriaLogs cannot open such a partition safely.
func isPartitionDir(dir string) (bool, error) {
	hasIndexdb, err := isDir(filepath.Join(dir, IndexdbDir))
	if err != nil {
		return false, err
	}
	hasDatadb, err := isDir(filepath.Join(dir, DatadbDir))
	if err != nil {
		return false, err
	}
	if hasIndexdb != hasDatadb {
		return false, fmt.Errorf("directory %q must contain both %q and %q directories", dir, IndexdbDir, DatadbDir)
	}
	return hasIndexdb, nil
}

func isDir(p string) (bool, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return fi.IsDir(), nil
}
