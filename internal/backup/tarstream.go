package backup

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
)

// tarStats describes the content written by writeTar.
type tarStats struct {
	Files     int
	Dirs      int
	Skipped   int
	DataBytes int64 // sum of regular file sizes
}

// writeTar writes the directory tree srcDir to w as a tar stream.
//
// Entry names are rootName/<path relative to srcDir> with forward slashes,
// matching `cd $(dirname srcDir) && tar -cf x.tar ./rootName` (without the
// leading "./"). Directories and regular files are stored; other file types
// (symlinks, devices, sockets, ...) are skipped with a warning.
//
// On error the tar stream is left unterminated, so a consumer cannot mistake
// it for a complete archive.
func writeTar(ctx context.Context, w io.Writer, srcDir, rootName string, log *slog.Logger) (tarStats, error) {
	var st tarStats
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		name := rootName
		if rel != "." {
			name = rootName + "/" + filepath.ToSlash(rel)
		}
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if err := writeTarHeader(tw, info, name+"/"); err != nil {
				return err
			}
			st.Dirs++
		case d.Type().IsRegular():
			n, err := writeTarFile(tw, path, name)
			if err != nil {
				return err
			}
			st.Files++
			st.DataBytes += n
		default:
			log.Warn("skipping unsupported file type", "path", path, "type", d.Type().String())
			st.Skipped++
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	if err := tw.Close(); err != nil {
		return st, fmt.Errorf("cannot finalize tar stream: %w", err)
	}
	return st, nil
}

func writeTarHeader(tw *tar.Writer, info fs.FileInfo, name string) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("cannot build tar header for %q: %w", name, err)
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("cannot write tar header for %q: %w", name, err)
	}
	return nil
}

func writeTarFile(tw *tar.Writer, path, name string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// Stat the opened file, so the header matches the content being read.
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("cannot stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%q is no longer a regular file", path)
	}
	if err := writeTarHeader(tw, info, name); err != nil {
		return 0, err
	}
	n, err := io.CopyN(tw, f, info.Size())
	if err != nil {
		return n, fmt.Errorf("cannot copy %q into tar stream (copied %d of %d bytes): %w", path, n, info.Size(), err)
	}
	return n, nil
}

// countingWriter counts bytes successfully written to w.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n.Add(int64(n))
	return n, err
}
