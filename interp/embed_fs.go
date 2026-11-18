package interp

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"
)

// embedFS is the read-only filesystem stored in an embed.FS variable.
// It implements fs.FS, fs.ReadFileFS, and fs.ReadDirFS.
type embedFS struct {
	files []embedFile // sorted by name, files only
}

type embedFile struct {
	name string
	data []byte
}

type embedFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (fi embedFileInfo) Name() string       { return fi.name }
func (fi embedFileInfo) Size() int64        { return fi.size }
func (fi embedFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi embedFileInfo) ModTime() time.Time { return time.Time{} }
func (fi embedFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi embedFileInfo) Sys() interface{}   { return nil }

type embedDirEntry struct {
	name string
	size int64
	mode fs.FileMode
}

func (d embedDirEntry) Name() string      { return d.name }
func (d embedDirEntry) IsDir() bool       { return d.mode.IsDir() }
func (d embedDirEntry) Type() fs.FileMode { return d.mode.Type() }
func (d embedDirEntry) Info() (fs.FileInfo, error) {
	return embedFileInfo{name: d.name, size: d.size, mode: d.mode}, nil
}

// embedOpenFile is a regular file opened from an embedFS.
type embedOpenFile struct {
	name   string
	data   []byte
	offset int64
}

func (f *embedOpenFile) Read(p []byte) (int, error) {
	if f.offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *embedOpenFile) Stat() (fs.FileInfo, error) {
	return embedFileInfo{name: path.Base(f.name), size: int64(len(f.data)), mode: 0444}, nil
}

func (f *embedOpenFile) Close() error { return nil }

// embedOpenDir is a directory opened from an embedFS.
// It implements fs.ReadDirFile.
type embedOpenDir struct {
	name    string
	entries []fs.DirEntry
	offset  int
}

func (d *embedOpenDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}

func (d *embedOpenDir) Stat() (fs.FileInfo, error) {
	name := d.name
	if name != "." {
		name = path.Base(d.name)
	}
	return embedFileInfo{name: name, mode: fs.ModeDir | 0555}, nil
}

func (d *embedOpenDir) Close() error { return nil }

func (d *embedOpenDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.offset >= len(d.entries) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	end := len(d.entries)
	if n > 0 && d.offset+n < end {
		end = d.offset + n
	}
	out := append([]fs.DirEntry(nil), d.entries[d.offset:end]...)
	d.offset = end
	return out, nil
}

var (
	_ fs.FS          = embedFS{}
	_ fs.ReadFileFS  = embedFS{}
	_ fs.ReadDirFS   = embedFS{}
	_ fs.File        = (*embedOpenFile)(nil)
	_ fs.File        = (*embedOpenDir)(nil)
	_ fs.ReadDirFile = (*embedOpenDir)(nil)
)

func embedFSFromMatches(fsys fs.FS, dir string, matches []string) (embedFS, error) {
	list := make([]embedFile, 0, len(matches))
	for _, m := range matches {
		data, err := fs.ReadFile(fsys, m)
		if err != nil {
			return embedFS{}, fmtEmbed(err)
		}
		buf := make([]byte, len(data))
		copy(buf, data)
		list = append(list, embedFile{name: embedRel(dir, m), data: buf})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	return embedFS{files: list}, nil
}

func fmtEmbed(err error) error {
	return errors.New("go:embed: " + err.Error())
}

func newEmbedFSValue(typ *itype, fsys embedFS) reflect.Value {
	v := reflect.New(typ.frameType()).Elem()
	v.Set(reflect.ValueOf(fsys))
	return v
}

func (f embedFS) lookup(name string) (embedFile, bool) {
	i := sort.Search(len(f.files), func(i int) bool { return f.files[i].name >= name })
	if i < len(f.files) && f.files[i].name == name {
		return f.files[i], true
	}
	return embedFile{}, false
}

func (f embedFS) hasDir(name string) bool {
	if name == "." {
		return true
	}
	prefix := name + "/"
	i := sort.Search(len(f.files), func(i int) bool { return f.files[i].name >= prefix })
	return i < len(f.files) && strings.HasPrefix(f.files[i].name, prefix)
}

func (f embedFS) dirEntries(name string) []fs.DirEntry {
	prefix := ""
	if name != "." {
		prefix = name + "/"
	}
	seen := map[string]bool{}
	var ents []embedDirEntry
	for _, file := range f.files {
		rest := file.name
		if prefix != "" {
			if !strings.HasPrefix(file.name, prefix) {
				continue
			}
			rest = file.name[len(prefix):]
		}
		elem, _, more := strings.Cut(rest, "/")
		if elem == "" || seen[elem] {
			continue
		}
		seen[elem] = true
		if more {
			ents = append(ents, embedDirEntry{name: elem, mode: fs.ModeDir | 0555})
		} else {
			ents = append(ents, embedDirEntry{name: elem, size: int64(len(file.data)), mode: 0444})
		}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].name < ents[j].name })
	out := make([]fs.DirEntry, len(ents))
	for i, e := range ents {
		out[i] = e
	}
	return out
}

func (f embedFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if file, ok := f.lookup(name); ok {
		return &embedOpenFile{name: name, data: file.data}, nil
	}
	if f.hasDir(name) {
		return &embedOpenDir{name: name, entries: f.dirEntries(name)}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (f embedFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	file, ok := f.lookup(name)
	if !ok {
		err := fs.ErrNotExist
		if f.hasDir(name) {
			err = errors.New("is a directory")
		}
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	// Each call returns its own slice. Callers may edit the result.
	out := make([]byte, len(file.data))
	copy(out, file.data)
	return out, nil
}

func (f embedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	if _, ok := f.lookup(name); ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	if name != "." && !f.hasDir(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return f.dirEntries(name), nil
}
