package interp

import (
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func mapFiles(files map[string]string) fstest.MapFS {
	fsys := make(fstest.MapFS, len(files))
	for name, data := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return fsys
}

func compileOn(t *testing.T, files map[string]string, name string) (*Interpreter, *Program) {
	t.Helper()
	i := New(Options{SourcecodeFilesystem: mapFiles(files)})
	p, err := i.CompilePath(name)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	i.resizeFrame()
	return i, p
}

func mustExec(t *testing.T, i *Interpreter, p *Program) {
	t.Helper()
	if _, err := i.Execute(p); err != nil {
		t.Fatal(err)
	}
}

func globalString(t *testing.T, i *Interpreter, name string) string {
	t.Helper()
	v, ok := i.Globals()[name]
	if !ok || !v.IsValid() || v.Kind() != reflect.String {
		t.Fatalf("global %s = %#v", name, v)
	}
	return v.String()
}

func globalBytes(t *testing.T, i *Interpreter, name string) []byte {
	t.Helper()
	v, ok := i.Globals()[name]
	if !ok || !v.IsValid() || v.Kind() != reflect.Slice {
		t.Fatalf("global %s = %#v", name, v)
	}
	if v.IsNil() {
		return nil
	}
	out := make([]byte, v.Len())
	copy(out, v.Bytes())
	return out
}

func globalFS(t *testing.T, i *Interpreter, name string) embedFS {
	t.Helper()
	v, ok := i.Globals()[name]
	if !ok || !v.IsValid() {
		t.Fatalf("global %s missing", name)
	}
	fsys, ok := v.Interface().(embedFS)
	if !ok {
		t.Fatalf("global %s type %T", name, v.Interface())
	}
	return fsys
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q, want substring %q", err.Error(), substr)
	}
}

func dirNames(t *testing.T, fsys embedFS, name string) []string {
	t.Helper()
	ents, err := fsys.ReadDir(name)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(ents))
	for i, e := range ents {
		out[i] = e.Name()
	}
	return out
}

func TestEmbedStringAndBytesBeforeStatements(t *testing.T) {
	files := map[string]string{
		"main.go": `package main

//go:embed hello.txt
var s string

//go:embed hello.txt
var u []uint8

var (
	//go:embed hello.txt
	b []byte
	//go:embed empty.bin
	empty []byte
	plain string
)

var copied = s

func init() {
	if s != "hello" {
		panic("s:" + s)
	}
	if string(b) != "hello" || string(u) != "hello" {
		panic("bytes")
	}
	if empty == nil {
		panic("nil empty")
	}
	s = s + "!"
}
`,
		"hello.txt": "hello",
		"empty.bin": "",
	}
	i, p := compileOn(t, files, "main.go")
	if got := globalString(t, i, "s"); got != "hello" {
		t.Fatalf("before statements s = %q", got)
	}
	if got := globalString(t, i, "copied"); got != "" {
		t.Fatalf("initializer ran early, copied = %q", got)
	}
	if got := globalBytes(t, i, "b"); string(got) != "hello" {
		t.Fatalf("before statements b = %q", got)
	}
	if got := globalBytes(t, i, "u"); string(got) != "hello" {
		t.Fatalf("before statements u = %q", got)
	}
	empty := i.Globals()["empty"]
	if empty.IsNil() || empty.Len() != 0 {
		t.Fatalf("empty []byte = nil:%v len:%d", empty.IsNil(), empty.Len())
	}
	if got := globalString(t, i, "plain"); got != "" {
		t.Fatalf("plain = %q", got)
	}
	i.Globals()["plain"].SetString("stale")

	mustExec(t, i, p)
	if got := globalString(t, i, "s"); got != "hello!" {
		t.Fatalf("after init s = %q", got)
	}
	if got := globalString(t, i, "copied"); got != "hello" {
		t.Fatalf("copied = %q", got)
	}
	if got := globalBytes(t, i, "b"); string(got) != "hello" {
		t.Fatalf("b wiped to %q", got)
	}
	if i.Globals()["empty"].IsNil() {
		t.Fatal("reset replaced empty []byte with nil")
	}
	if got := globalString(t, i, "plain"); got != "" {
		t.Fatalf("plain reset left %q", got)
	}
}

func TestEmbedGroupAndPlainVar(t *testing.T) {
	plain := map[string]string{
		"main.go": `package main

//go:embed hello.txt
var s string
`,
		"hello.txt": "hello",
	}
	i, _ := compileOn(t, plain, "main.go")
	if got := globalString(t, i, "s"); got != "hello" {
		t.Fatalf("plain var = %q", got)
	}

	group := map[string]string{
		"main.go": `package main

var (
	//go:embed hello.txt
	s string
	//go:embed hello.txt
	b []byte
	other string
)
`,
		"hello.txt": "hello",
	}
	i, p := compileOn(t, group, "main.go")
	if got := globalString(t, i, "s"); got != "hello" {
		t.Fatalf("group s = %q", got)
	}
	if got := globalBytes(t, i, "b"); string(got) != "hello" {
		t.Fatalf("group b = %q", got)
	}
	if got := globalString(t, i, "other"); got != "" {
		t.Fatalf("other = %q", got)
	}
	mustExec(t, i, p)
	if got := globalString(t, i, "s"); got != "hello" {
		t.Fatalf("after reset s = %q", got)
	}
	if got := globalString(t, i, "other"); got != "" {
		t.Fatalf("after reset other = %q", got)
	}
}

func TestEmbedAliases(t *testing.T) {
	files := map[string]string{
		"main.go": `package main

type S = string
type B = []byte

//go:embed hello.txt
var s S

//go:embed hello.txt
var b B
`,
		"hello.txt": "hello",
	}
	i, _ := compileOn(t, files, "main.go")
	if got := globalString(t, i, "s"); got != "hello" {
		t.Fatalf("alias string = %q", got)
	}
	if got := globalBytes(t, i, "b"); string(got) != "hello" {
		t.Fatalf("alias bytes = %q", got)
	}
}

func TestEmbedFSBeforeStatements(t *testing.T) {
	src := `package main

import "embed"

//go:embed data
//go:embed README.md
var files embed.FS

var plain string

func init() {
	b, err := files.ReadFile("README.md")
	if err != nil {
		panic(err)
	}
	if string(b) != "virtual-not-disk" {
		panic(string(b))
	}
	b[0] = 'X'
	b, err = files.ReadFile("README.md")
	if err != nil {
		panic(err)
	}
	if string(b) != "virtual-not-disk" {
		panic("shared:" + string(b))
	}
	b, err = files.ReadFile("data/sub/b.txt")
	if err != nil {
		panic(err)
	}
	if string(b) != "nested" {
		panic(string(b))
	}
	ents, err := files.ReadDir("data")
	if err != nil {
		panic(err)
	}
	if len(ents) != 3 || ents[0].Name() != "a.txt" || ents[1].Name() != "m.txt" || ents[2].Name() != "sub" {
		panic("dir")
	}
}
`
	files := map[string]string{
		"main.go":        src,
		"README.md":      "virtual-not-disk",
		"data/a.txt":     "A",
		"data/m.txt":     "M",
		"data/sub/b.txt": "nested",
		"data/.hid":      "hidden",
		"data/_x":        "under",
	}
	i, p := compileOn(t, files, "main.go")
	fsys := globalFS(t, i, "files")
	checkEmbedTree(t, fsys)
	if got := globalString(t, i, "plain"); got != "" {
		t.Fatalf("plain = %q", got)
	}
	i.Globals()["plain"].SetString("stale")
	mustExec(t, i, p)
	if got := globalString(t, i, "plain"); got != "" {
		t.Fatalf("plain reset left %q", got)
	}
	checkEmbedTree(t, globalFS(t, i, "files"))
}

func TestEmbedFSGroupCombinesLines(t *testing.T) {
	files := map[string]string{
		"main.go": `package main

import "embed"

var (
	//go:embed hello.txt
	//go:embed data/sub/b.txt
	files embed.FS
)
`,
		"hello.txt":      "hello",
		"data/sub/b.txt": "nested",
		"data/a.txt":     "skip-me",
	}
	i, _ := compileOn(t, files, "main.go")
	fsys := globalFS(t, i, "files")
	b, err := fsys.ReadFile("hello.txt")
	if err != nil || string(b) != "hello" {
		t.Fatalf("hello = %q %v", b, err)
	}
	b, err = fsys.ReadFile("data/sub/b.txt")
	if err != nil || string(b) != "nested" {
		t.Fatalf("nested = %q %v", b, err)
	}
	if _, err := fsys.ReadFile("data/a.txt"); err == nil {
		t.Fatal("pattern that was not named was included")
	}
}

func TestEmbedFSStarIsOneLevel(t *testing.T) {
	files := map[string]string{
		"main.go": `package main

import "embed"

//go:embed data/*
var files embed.FS
`,
		"data/a.txt":     "A",
		"data/sub/b.txt": "nested",
	}
	i, _ := compileOn(t, files, "main.go")
	fsys := globalFS(t, i, "files")
	if b, err := fsys.ReadFile("data/a.txt"); err != nil || string(b) != "A" {
		t.Fatalf("a = %q %v", b, err)
	}
	if _, err := fsys.ReadFile("data/sub/b.txt"); err == nil {
		t.Fatal("star pattern included a nested file")
	}
}

func TestEmbedFSRelativeDir(t *testing.T) {
	files := map[string]string{
		"sub/main.go": `package main

import "embed"

//go:embed data
var files embed.FS
`,
		"sub/data/a.txt":        "sub-a",
		"sub/data/nested/b.txt": "sub-b",
		"data/a.txt":            "root-a",
		"hello.txt":             "root-hello",
	}
	i, _ := compileOn(t, files, "sub/main.go")
	fsys := globalFS(t, i, "files")
	if b, err := fsys.ReadFile("data/a.txt"); err != nil || string(b) != "sub-a" {
		t.Fatalf("relative file = %q %v", b, err)
	}
	if b, err := fsys.ReadFile("data/nested/b.txt"); err != nil || string(b) != "sub-b" {
		t.Fatalf("relative nested = %q %v", b, err)
	}
}

func TestEmbedSourceFSNotDisk(t *testing.T) {
	files := map[string]string{
		"main.go": `package main

//go:embed README.md
var s string
`,
		"README.md": "virtual-not-disk",
	}
	i, _ := compileOn(t, files, "main.go")
	if got := globalString(t, i, "s"); got != "virtual-not-disk" {
		t.Fatalf("s = %q", got)
	}
}

func TestEmbedImportSrc(t *testing.T) {
	files := map[string]string{
		"main.go": `package main

import "pkg"

func init() {
	b, err := pkg.Files.ReadFile("msg.txt")
	if err != nil {
		panic(err)
	}
	if string(b) != "from-pkg" {
		panic(string(b))
	}
	if pkg.Snap != "from-pkg" || pkg.Msg != "from-pkg" {
		panic(pkg.Snap + pkg.Msg)
	}
}
`,
		"_pkg/src/pkg/pkg.go": `package pkg

import "embed"

//go:embed msg.txt
var Files embed.FS

//go:embed msg.txt
var Msg string

var Snap = Msg
`,
		"_pkg/src/pkg/msg.txt": "from-pkg",
	}
	i := New(Options{GoPath: "./_pkg", SourcecodeFilesystem: mapFiles(files)})
	if _, err := i.EvalPath("main.go"); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedLinesAreCombined(t *testing.T) {
	files := map[string]string{
		"main.go": `package main

//go:embed a.txt
//go:embed b.txt
var s string
`,
		"a.txt": "A",
		"b.txt": "B",
	}
	i := New(Options{SourcecodeFilesystem: mapFiles(files)})
	_, err := i.CompilePath("main.go")
	wantErr(t, err, "go:embed: pattern must match exactly one file")
}

func TestEmbedErrors(t *testing.T) {
	hello := map[string]string{"hello.txt": "hello", "a.txt": "A", "b.txt": "B", "dir/a.txt": "A", "dir/b.txt": "B"}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "missing",
			src:  "//go:embed missing.txt\nvar s string\n",
			want: "go:embed: no matching files found",
		},
		{
			name: "glob two",
			src:  "//go:embed *.txt\nvar s string\n",
			want: "go:embed: pattern must match exactly one file",
		},
		{
			name: "directory two",
			src:  "//go:embed dir\nvar s string\n",
			want: "go:embed: pattern must match exactly one file",
		},
		{
			name: "dotdot",
			src:  "//go:embed ../hello.txt\nvar s string\n",
			want: `go:embed: invalid pattern "../hello.txt"`,
		},
		{
			name: "initializer",
			src:  "//go:embed hello.txt\nvar s string = \"x\"\n",
			want: "go:embed cannot apply to a variable with an initializer",
		},
		{
			name: "multiple names",
			src:  "//go:embed hello.txt\nvar a, b string\n",
			want: "go:embed cannot apply to multiple variables",
		},
		{
			name: "group two",
			src:  "//go:embed hello.txt\nvar (\n\ta string\n\tb string\n)\n",
			want: "go:embed cannot apply to multiple variables",
		},
		{
			name: "spec two names",
			src:  "var (\n\t//go:embed hello.txt\n\ta, b string\n)\n",
			want: "go:embed cannot apply to multiple variables",
		},
		{
			name: "int",
			src:  "//go:embed hello.txt\nvar n int\n",
			want: "go:embed cannot apply to var of type int",
		},
		{
			name: "named string",
			src:  "type S string\n//go:embed hello.txt\nvar s S\n",
			want: "go:embed cannot apply to var of type main.S",
		},
		{
			name: "named bytes",
			src:  "type B []byte\n//go:embed hello.txt\nvar b B\n",
			want: "go:embed cannot apply to var of type main.B",
		},
		{
			name: "local",
			src:  "func f() {\n\t//go:embed hello.txt\n\tvar s string\n}\n",
			want: "go:embed only allowed at package level",
		},
		{
			name: "on func",
			src:  "//go:embed hello.txt\nfunc f() {}\n",
			want: "misplaced go:embed directive",
		},
		{
			name: "blank line",
			src:  "//go:embed hello.txt\n\nvar s string\n",
			want: "misplaced go:embed directive",
		},
		{
			name: "missing pattern",
			src:  "//go:embed\nvar s string\n",
			want: "go:embed: missing file pattern",
		},
		{
			name: "empty pattern",
			src:  "//go:embed \"\"\nvar s string\n",
			want: "go:embed: empty pattern",
		},
		{
			name: "fs missing pattern not skipped",
			src:  "import \"embed\"\n//go:embed hello.txt\n//go:embed missing.txt\nvar files embed.FS\n",
			want: "go:embed: no matching files found",
		},
		{
			name: "not a directive",
			src:  "//go:embedded hello.txt\nvar s string\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"main.go": "package main\n" + tc.src}
			for k, v := range hello {
				files[k] = v
			}
			i := New(Options{SourcecodeFilesystem: mapFiles(files)})
			_, err := i.CompilePath("main.go")
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				i.resizeFrame()
				if got := globalString(t, i, "s"); got != "" {
					t.Fatalf("non-directive filled s with %q", got)
				}
				return
			}
			wantErr(t, err, tc.want)
		})
	}
}

func TestMatchEmbedPatterns(t *testing.T) {
	fsys := mapFiles(map[string]string{
		"a.txt":         "a",
		"b.txt":         "b",
		"dir/a.txt":     "A",
		"dir/sub/b.txt": "B",
		"dir/.hid":      "H",
		"dir/_x":        "U",
		"_explicit.txt": "E",
	})
	cases := []struct {
		name     string
		patterns []string
		want     []string
		wantErr  string
	}{
		{name: "tree", patterns: []string{"dir"}, want: []string{"dir/a.txt", "dir/sub/b.txt"}},
		{name: "star one level", patterns: []string{"dir/*"}, want: []string{"dir/a.txt"}},
		{name: "dedupe", patterns: []string{"a.txt", "a.txt"}, want: []string{"a.txt"}},
		{name: "all hidden", patterns: []string{"all:dir"}, want: []string{"dir/.hid", "dir/_x", "dir/a.txt", "dir/sub/b.txt"}},
		{name: "explicit hidden", patterns: []string{"_explicit.txt"}, want: []string{"_explicit.txt"}},
		{name: "miss", patterns: []string{"a.txt", "nope.txt"}, wantErr: "go:embed: no matching files found"},
		{name: "glob miss", patterns: []string{"*.nope"}, wantErr: "go:embed: no matching files found"},
		{name: "dotdot", patterns: []string{"../a.txt"}, wantErr: "invalid pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchEmbedPatterns(fsys, ".", tc.patterns)
			if tc.wantErr != "" {
				wantErr(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestEmbedFSCopyTreeAndReadDir(t *testing.T) {
	fsys := mapFiles(map[string]string{
		"z.txt":          "Z",
		"a.txt":          "A",
		"m.txt":          "M",
		"data/a.txt":     "da",
		"data/m.txt":     "dm",
		"data/sub/b.txt": "nested",
		"data/.hid":      "H",
	})
	got, err := buildEmbedFS(fsys, ".", []string{"data", "z.txt", "a.txt", "m.txt"})
	if err != nil {
		t.Fatal(err)
	}
	checkEmbedTree(t, got)
	if names := dirNames(t, got, "."); strings.Join(names, ",") != "a.txt,data,m.txt,z.txt" {
		t.Fatalf("root listing %v", names)
	}
	first, err := got.ReadFile("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'X'
	second, err := got.ReadFile("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != "A" || string(first) == string(second) {
		t.Fatalf("ReadFile handed back shared bytes %q %q", first, second)
	}
	if _, err := got.ReadFile("data/.hid"); err == nil {
		t.Fatal("hidden file included without all:")
	}

	f, err := got.Open("data")
	if err != nil {
		t.Fatal(err)
	}
	rdf, ok := f.(fs.ReadDirFile)
	if !ok {
		t.Fatalf("directory file %T does not implement fs.ReadDirFile", f)
	}
	one, err := rdf.ReadDir(1)
	if err != nil || len(one) != 1 || one[0].Name() != "a.txt" {
		t.Fatalf("first ReadDir %v %v", one, err)
	}
	rest, err := rdf.ReadDir(0)
	if err != nil || len(rest) != 2 || rest[0].Name() != "m.txt" || rest[1].Name() != "sub" {
		t.Fatalf("rest %v %v", rest, err)
	}
	if _, err := rdf.ReadDir(1); err != io.EOF {
		t.Fatalf("after end got %v", err)
	}

	file, err := got.Open("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.(fs.ReadDirFile); ok {
		t.Fatal("regular file implements ReadDirFile")
	}
}

func checkEmbedTree(t *testing.T, fsys embedFS) {
	t.Helper()
	var asFS fs.FS = fsys
	var asRead fs.ReadFileFS = fsys
	var asDir fs.ReadDirFS = fsys
	_ = asFS
	_ = asRead
	_ = asDir

	b, err := fsys.ReadFile("README.md")
	if err != nil || string(b) != "virtual-not-disk" {
		// buildEmbedFS tests do not include README. Only check it when present.
		if _, ok := fsys.lookup("README.md"); ok {
			t.Fatalf("README = %q %v", b, err)
		}
	}
	if _, ok := fsys.lookup("README.md"); ok {
		b[0] = 'X'
		again, err := fs.ReadFile(fsys, "README.md")
		if err != nil || string(again) != "virtual-not-disk" {
			t.Fatalf("fs.ReadFile shared or changed %q %v", again, err)
		}
	}
	if b, err := fsys.ReadFile("data/sub/b.txt"); err != nil || string(b) != "nested" {
		if _, ok := fsys.lookup("data/sub/b.txt"); ok || string(b) != "" {
			t.Fatalf("nested = %q %v", b, err)
		}
	}
	if _, ok := fsys.lookup("data/a.txt"); ok {
		if names := dirNames(t, fsys, "data"); strings.Join(names, ",") != "a.txt,m.txt,sub" {
			t.Fatalf("data listing %v", names)
		}
		f, err := fsys.Open("data")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := f.(fs.ReadDirFile); !ok {
			t.Fatalf("%T is not fs.ReadDirFile", f)
		}
		file, err := fsys.Open("data/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := file.(fs.ReadDirFile); ok {
			t.Fatal("file implements ReadDirFile")
		}
	}
}

// buildEmbedFS matches patterns and reads them, like the directive does for embed.FS.
func buildEmbedFS(fsys fs.FS, dir string, patterns []string) (embedFS, error) {
	matches, err := matchEmbedPatterns(fsys, dir, patterns)
	if err != nil {
		return embedFS{}, err
	}
	return embedFSFromMatches(fsys, dir, matches)
}
