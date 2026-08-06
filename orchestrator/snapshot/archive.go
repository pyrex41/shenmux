package snapshot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// TarGzDir walks root and returns a deterministic tar.gz of its contents.
// Entries are emitted sorted by relative path so the same tree always produces
// the same archive bytes (and thus the same content address). Regular files,
// directories and symlinks are captured; mtimes are zeroed to keep the archive
// content-stable across restores.
func TarGzDir(root string) ([]byte, error) {
	type item struct {
		rel  string
		path string
		d    fs.DirEntry
	}
	var items []item
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		items = append(items, item{rel: filepath.ToSlash(rel), path: path, d: d})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Deterministic order.
	sortItems := func() {
		for i := 1; i < len(items); i++ {
			for j := i; j > 0 && items[j-1].rel > items[j].rel; j-- {
				items[j-1], items[j] = items[j], items[j-1]
			}
		}
	}
	sortItems()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, it := range items {
		info, err := it.d.Info()
		if err != nil {
			return nil, err
		}
		var link string
		if it.d.Type()&fs.ModeSymlink != 0 {
			link, err = os.Readlink(it.path)
			if err != nil {
				return nil, err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil, err
		}
		hdr.Name = it.rel
		if it.d.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		hdr.ModTime = time0()
		hdr.AccessTime = time0()
		hdr.ChangeTime = time0()
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if it.d.Type().IsRegular() {
			f, err := os.Open(it.path)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return nil, err
			}
			f.Close()
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UntarGz extracts a tar.gz byte stream into dest, creating it if needed.
func UntarGz(data []byte, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// safeJoin joins dest and name, rejecting paths that escape dest (zip-slip).
func safeJoin(dest, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(dest, name))
	prefix := filepath.Clean(dest) + string(os.PathSeparator)
	if clean != filepath.Clean(dest) && !strings.HasPrefix(clean, prefix) {
		return "", &os.PathError{Op: "extract", Path: name, Err: fs.ErrInvalid}
	}
	return clean, nil
}
