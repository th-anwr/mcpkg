package scan

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Limits protect against zip bombs and huge packages.
type Limits struct {
	MaxFiles int
	MaxBytes int64
}

// DefaultLimits are generous for MCP servers.
var DefaultLimits = Limits{MaxFiles: 20000, MaxBytes: 300 << 20}

var errLimit = errors.New("archive exceeds extraction limits")

// safeJoin rejects absolute paths and traversal ("zip slip").
func safeJoin(dst, name string) (string, error) {
	name = filepath.FromSlash(strings.TrimPrefix(name, "/"))
	p := filepath.Join(dst, name)
	if p != dst && !strings.HasPrefix(p, filepath.Clean(dst)+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return p, nil
}

// ExtractTarGz extracts a .tgz/.tar.gz stream (regular files and dirs only; links are skipped).
func ExtractTarGz(r io.Reader, dst string, lim Limits) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files int
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := safeJoin(dst, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			total += h.Size
			if files > lim.MaxFiles || total > lim.MaxBytes {
				return errLimit
			}
			if err := writeFile(p, io.LimitReader(tr, h.Size)); err != nil {
				return err
			}
		}
	}
}

// ExtractZip extracts a zip/wheel file.
func ExtractZip(path, dst string, lim Limits) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	var total int64
	if len(zr.File) > lim.MaxFiles {
		return errLimit
	}
	for _, f := range zr.File {
		p, err := safeJoin(dst, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			continue
		}
		total += int64(f.UncompressedSize64)
		if total > lim.MaxBytes {
			return errLimit
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(p, io.LimitReader(rc, int64(f.UncompressedSize64)))
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(p string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
