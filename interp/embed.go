package interp

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Package-level //go:embed fills a string or []byte variable from the
// interpreter source filesystem. Patterns are relative to the directory of
// the Go file that contains the directive. Those two types require the
// patterns, taken together, to match exactly one file.

func (interp *Interpreter) posError(pos token.Pos, format string, args ...interface{}) error {
	p := interp.fset.Position(pos)
	posString := p.String()
	if p.Filename == DefaultSourceName {
		posString = strings.TrimPrefix(posString, DefaultSourceName+":")
	}
	args = append([]interface{}{posString}, args...)
	return fmt.Errorf("%s: "+format, args...)
}

func (interp *Interpreter) checkEmbedPlacement(file *ast.File) error {
	for _, g := range file.Comments {
		if !commentHasEmbed(g) {
			continue
		}
		if !embedCommentOnVar(file, g) {
			return interp.posError(g.Pos(), "misplaced go:embed directive")
		}
	}
	return nil
}

func embedCommentOnVar(file *ast.File, g *ast.CommentGroup) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return true
		}
		if gd.Doc == g {
			found = true
			return false
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if ok && vs.Doc == g {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func (interp *Interpreter) attachDeclEmbed(n *node, doc *ast.CommentGroup, parent *node, tok token.Token) error {
	if tok != token.VAR {
		if commentHasEmbed(doc) {
			return interp.posError(doc.Pos(), "misplaced go:embed directive")
		}
		return nil
	}
	pats, err := patternsFromComment(doc)
	if err != nil {
		return interp.posError(doc.Pos(), "%s", err.Error())
	}
	n.embedPatterns = pats
	if len(pats) == 0 {
		return nil
	}
	if parent == nil || parent.kind != fileStmt {
		return interp.posError(n.pos, "go:embed only allowed at package level")
	}
	return nil
}

func (interp *Interpreter) attachSpecEmbed(n *node, doc *ast.CommentGroup, parent *node) error {
	pats, err := patternsFromComment(doc)
	if err != nil {
		pos := n.pos
		if doc != nil {
			pos = doc.Pos()
		}
		return interp.posError(pos, "%s", err.Error())
	}
	n.embedPatterns = pats
	if len(pats) == 0 {
		return nil
	}
	if parent == nil || parent.kind != varDecl {
		return interp.posError(doc.Pos(), "misplaced go:embed directive")
	}
	if parent.anc == nil || parent.anc.kind != fileStmt {
		return interp.posError(n.pos, "go:embed only allowed at package level")
	}
	return nil
}

// embedDirectives returns the //go:embed patterns that apply to spec n.
// Patterns on the var keyword apply only when that declaration introduces
// a single variable. Patterns on the spec apply to that spec alone.
func (n *node) embedDirectives() ([]string, error) {
	var pats []string
	if n.anc != nil && n.anc.kind == varDecl && len(n.anc.embedPatterns) > 0 {
		nvars := 0
		for _, c := range n.anc.child {
			nvars += c.nleft
		}
		if nvars != 1 {
			return nil, n.cfgErrorf("go:embed cannot apply to multiple variables")
		}
		pats = append(pats, n.anc.embedPatterns...)
	}
	if len(n.embedPatterns) > 0 && n.nleft != 1 {
		return nil, n.cfgErrorf("go:embed cannot apply to multiple variables")
	}
	pats = append(pats, n.embedPatterns...)
	return pats, nil
}

// rejectEmbedOnInitializer reports directives on variables that are not a
// plain package-level var. Those are initializers or non-var declarations
// that still carried a pattern onto the node.
func (n *node) rejectEmbedOnInitializer() error {
	if len(n.embedPatterns) == 0 && (n.anc == nil || len(n.anc.embedPatterns) == 0) {
		return nil
	}
	if _, err := n.embedDirectives(); err != nil {
		return err
	}
	if n.anc == nil || n.anc.kind != varDecl || n.anc.anc == nil || n.anc.anc.kind != fileStmt {
		return n.cfgErrorf("go:embed only allowed at package level")
	}
	return n.cfgErrorf("go:embed cannot apply to a variable with an initializer")
}

func (interp *Interpreter) loadEmbedValue(n *node, typ *itype, patterns []string) (reflect.Value, error) {
	if !isEmbedString(typ) && !isEmbedBytes(typ) {
		return reflect.Value{}, n.cfgErrorf("go:embed cannot apply to var of type %s", typ.id())
	}
	filename := filepath.ToSlash(interp.fset.Position(n.pos).Filename)
	matches, err := matchEmbedPatterns(interp.filesystem, path.Dir(filename), patterns)
	if err != nil {
		return reflect.Value{}, n.cfgErrorf("%s", err.Error())
	}
	switch len(matches) {
	case 0:
		return reflect.Value{}, n.cfgErrorf("go:embed: no matching files found")
	case 1:
	default:
		return reflect.Value{}, n.cfgErrorf("go:embed: pattern must match exactly one file")
	}
	data, err := fs.ReadFile(interp.filesystem, matches[0])
	if err != nil {
		return reflect.Value{}, n.cfgErrorf("go:embed: %s", err.Error())
	}
	return newEmbedValue(typ, data), nil
}

func (interp *Interpreter) setEmbedValue(index int, v reflect.Value) {
	if interp.embedValues == nil {
		interp.embedValues = make(map[int]reflect.Value)
	}
	interp.embedValues[index] = v
}

// keepEmbed reports whether a global frame slot was filled by //go:embed.
// The plain var reset must leave that slot alone.
func keepEmbed(interp *Interpreter, index int) bool {
	if interp == nil || interp.embedValues == nil {
		return false
	}
	_, ok := interp.embedValues[index]
	return ok
}

func isEmbedString(t *itype) bool {
	return t != nil && t.cat == stringT && t.path == "" && t.name == "string" && !t.untyped
}

func isEmbedBytes(t *itype) bool {
	if t == nil || t.cat != sliceT || t.path != "" || t.name != "" || t.val == nil {
		return false
	}
	v := t.val
	return v.cat == uint8T && v.path == "" && v.name == "uint8"
}

func newEmbedValue(typ *itype, data []byte) reflect.Value {
	v := reflect.New(typ.frameType()).Elem()
	if isEmbedString(typ) {
		v.SetString(string(data))
		return v
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	v.SetBytes(buf)
	return v
}

func commentHasEmbed(g *ast.CommentGroup) bool {
	if g == nil {
		return false
	}
	for _, c := range g.List {
		if _, ok := directiveArgs(c.Text); ok {
			return true
		}
	}
	return false
}

func patternsFromComment(g *ast.CommentGroup) ([]string, error) {
	if g == nil {
		return nil, nil
	}
	var out []string
	for _, c := range g.List {
		args, ok := directiveArgs(c.Text)
		if !ok {
			continue
		}
		args = strings.TrimSpace(args)
		if args == "" {
			return nil, fmt.Errorf("go:embed: missing file pattern")
		}
		pats, err := parseEmbedArgs(args)
		if err != nil {
			return nil, err
		}
		out = append(out, pats...)
	}
	return out, nil
}

func directiveArgs(text string) (string, bool) {
	const prefix = "//go:embed"
	if !strings.HasPrefix(text, prefix) {
		return "", false
	}
	if len(text) == len(prefix) {
		return "", true
	}
	switch text[len(prefix)] {
	case ' ', '\t':
		return text[len(prefix)+1:], true
	default:
		return "", false
	}
}

func parseEmbedArgs(args string) ([]string, error) {
	var list []string
	for {
		args = strings.TrimLeft(args, " \t")
		if args == "" {
			break
		}
		var pattern string
		switch args[0] {
		case '`':
			end := strings.IndexByte(args[1:], '`')
			if end < 0 {
				return nil, fmt.Errorf("go:embed: invalid quoted string")
			}
			pattern = args[1 : 1+end]
			args = args[1+end+1:]
		case '"':
			i := 1
			for i < len(args) {
				if args[i] == '\\' {
					i += 2
					continue
				}
				if args[i] == '"' {
					break
				}
				i++
			}
			if i >= len(args) || args[i] != '"' {
				return nil, fmt.Errorf("go:embed: invalid quoted string")
			}
			s, err := strconv.Unquote(args[:i+1])
			if err != nil {
				return nil, fmt.Errorf("go:embed: invalid quoted string")
			}
			pattern = s
			args = args[i+1:]
		default:
			i := 0
			for i < len(args) && args[i] != ' ' && args[i] != '\t' {
				i++
			}
			pattern = args[:i]
			args = args[i:]
		}
		if pattern == "" {
			return nil, fmt.Errorf("go:embed: empty pattern")
		}
		list = append(list, pattern)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("go:embed: missing file pattern")
	}
	return list, nil
}

func matchEmbedPatterns(fsys fs.FS, dir string, patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, pattern := range patterns {
		matches, err := matchEmbed(fsys, dir, pattern)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

func matchEmbed(fsys fs.FS, dir, pattern string) ([]string, error) {
	all := false
	if strings.HasPrefix(pattern, "all:") {
		all = true
		pattern = strings.TrimPrefix(pattern, "all:")
	}
	if err := validEmbedPattern(pattern); err != nil {
		return nil, err
	}
	if embedHasMeta(pattern) {
		return matchEmbedGlob(fsys, dir, pattern, all)
	}
	return matchEmbedLiteral(fsys, dir, pattern, all)
}

func validEmbedPattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("go:embed: empty pattern")
	}
	if strings.Contains(pattern, "\\") {
		return fmt.Errorf("go:embed: invalid pattern %q", pattern)
	}
	if pattern[0] == '/' || strings.HasSuffix(pattern, "/") {
		return fmt.Errorf("go:embed: invalid pattern %q", pattern)
	}
	for _, elem := range strings.Split(pattern, "/") {
		if elem == "" || elem == "." || elem == ".." {
			return fmt.Errorf("go:embed: invalid pattern %q", pattern)
		}
		if _, err := path.Match(elem, ""); err != nil {
			return fmt.Errorf("go:embed: invalid pattern %q", pattern)
		}
	}
	return nil
}

func embedHasMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func embedJoin(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return path.Join(dir, name)
}

func embedRel(dir, full string) string {
	if dir == "" || dir == "." {
		return full
	}
	if full == dir {
		return ""
	}
	prefix := dir + "/"
	if strings.HasPrefix(full, prefix) {
		return full[len(prefix):]
	}
	return full
}

func embedHidden(name string) bool {
	return name != "" && (name[0] == '.' || name[0] == '_')
}

// embedNamesHidden reports whether every hidden element of rel is spelled
// out by the pattern, with no wildcard in that element.
func embedNamesHidden(pattern, rel string) bool {
	pElems := strings.Split(pattern, "/")
	rElems := strings.Split(rel, "/")
	for i, elem := range rElems {
		if !embedHidden(elem) {
			continue
		}
		if i >= len(pElems) || embedHasMeta(pElems[i]) || pElems[i] != elem {
			return false
		}
	}
	return true
}

func matchEmbedLiteral(fsys fs.FS, dir, pattern string, all bool) ([]string, error) {
	full := embedJoin(dir, pattern)
	fi, err := fs.Stat(fsys, full)
	if err != nil {
		if isNotExist(err) {
			return nil, fmt.Errorf("go:embed: no matching files found")
		}
		return nil, fmt.Errorf("go:embed: %s", err.Error())
	}
	if !fi.IsDir() {
		return []string{full}, nil
	}
	var matches []string
	err = fs.WalkDir(fsys, full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != full && embedHidden(d.Name()) && !all {
				return fs.SkipDir
			}
			return nil
		}
		if embedHidden(d.Name()) && !all {
			return nil
		}
		matches = append(matches, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("go:embed: %s", err.Error())
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("go:embed: no matching files found")
	}
	sort.Strings(matches)
	return matches, nil
}

func matchEmbedGlob(fsys fs.FS, dir, pattern string, all bool) ([]string, error) {
	prefix := pattern
	if i := strings.IndexAny(pattern, "*?["); i >= 0 {
		prefix = pattern[:i]
	}
	if j := strings.LastIndex(prefix, "/"); j >= 0 {
		prefix = prefix[:j]
	} else {
		prefix = ""
	}
	walkRoot := dir
	if walkRoot == "" {
		walkRoot = "."
	}
	if prefix != "" {
		walkRoot = embedJoin(dir, prefix)
	}
	if _, err := fs.Stat(fsys, walkRoot); err != nil {
		if isNotExist(err) {
			return nil, fmt.Errorf("go:embed: no matching files found")
		}
		return nil, fmt.Errorf("go:embed: %s", err.Error())
	}
	var matches []string
	err := fs.WalkDir(fsys, walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := embedRel(dir, p)
		if rel == "" || rel == "." {
			return nil
		}
		if embedHidden(d.Name()) && !all && !embedNamesHidden(pattern, rel) {
			if d.IsDir() && p != walkRoot {
				return fs.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		ok, err := path.Match(pattern, rel)
		if err != nil {
			return err
		}
		if ok {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("go:embed: %s", err.Error())
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("go:embed: no matching files found")
	}
	sort.Strings(matches)
	return matches, nil
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
