package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Manifest is the on-disk snapshot description. It is a superset of the
// autopoiesis snapshot-to-sexpr plist: the base fields (version/id/timestamp/
// parent/agent-state/metadata/hash) match autopoiesis, and tree-root,
// tree-entries and upperdir-archive are the added filesystem-state fields that
// autopoiesis drops on serialize.
type Manifest struct {
	Version         int
	ID              string
	Timestamp       int64
	Parent          string     // "" encodes nil
	Metadata        [][2]string // ordered (keyword-without-colon, value) pairs
	Hash            string     // sexpr-hash of the (nil) agent-state
	TreeRoot        string
	TreeEntries     []Entry
	UpperdirArchive string
}

// NewManifest builds a manifest with the standard metadata and the fixed
// nil-agent-state hash.
func NewManifest(id string, timestamp int64, parent string, treeRoot string, entries []Entry, archive string) *Manifest {
	return &Manifest{
		Version:         1,
		ID:              id,
		Timestamp:       timestamp,
		Parent:          parent,
		Metadata:        [][2]string{{"tool", "shenmux-orchestrator"}},
		Hash:            AgentStateHash(),
		TreeRoot:        treeRoot,
		TreeEntries:     entries,
		UpperdirArchive: archive,
	}
}

// Encode renders the manifest as a deterministic, human-readable s-expression.
func (m *Manifest) Encode() string {
	var b strings.Builder
	b.WriteString("(snapshot\n")
	b.WriteString("  :version " + strconv.Itoa(m.Version) + "\n")
	b.WriteString("  :id " + quote(m.ID) + "\n")
	b.WriteString("  :timestamp " + strconv.FormatInt(m.Timestamp, 10) + "\n")
	b.WriteString("  :parent " + optString(m.Parent) + "\n")
	b.WriteString("  :agent-state nil\n")
	b.WriteString("  :metadata (")
	for i, kv := range m.Metadata {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(":" + kv[0] + " " + quote(kv[1]))
	}
	b.WriteString(")\n")
	b.WriteString("  :hash " + quote(m.Hash) + "\n")
	b.WriteString("  :tree-root " + quote(m.TreeRoot) + "\n")
	b.WriteString("  :tree-entries (")
	for _, e := range m.TreeEntries {
		b.WriteString("\n    (:file " + quote(e.Path) +
			" :hash " + quote(e.Hash) +
			" :mode " + strconv.FormatInt(e.Mode, 10) +
			" :size " + strconv.FormatInt(e.Size, 10) +
			" :mtime " + strconv.FormatInt(e.Mtime, 10) + ")")
	}
	if len(m.TreeEntries) > 0 {
		b.WriteString("\n  ")
	}
	b.WriteString(")\n")
	b.WriteString("  :upperdir-archive " + optString(m.UpperdirArchive) + ")\n")
	return b.String()
}

func optString(s string) string {
	if s == "" {
		return "nil"
	}
	return quote(s)
}

func quote(s string) string { return strconv.Quote(s) }

// WriteManifest encodes the manifest and writes it to path, creating parents.
func WriteManifest(path string, m *Manifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(m.Encode()), 0o644)
}

// ReadManifest parses a manifest file from disk.
func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

// ParseManifest parses manifest s-expression bytes into a Manifest, recovering
// id/parent/tree-root/tree-entries/archive (and the rest of the base plist).
func ParseManifest(data []byte) (*Manifest, error) {
	p := &sexprParser{src: []rune(string(data))}
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if !v.isList || len(v.list) == 0 || v.list[0].sym != "snapshot" {
		return nil, fmt.Errorf("not a snapshot s-expression")
	}
	m := &Manifest{}
	plist := v.list[1:]
	for i := 0; i+1 < len(plist); i += 2 {
		key := plist[i].sym
		val := plist[i+1]
		switch key {
		case ":version":
			m.Version = atoiOr(val.sym, 0)
		case ":id":
			m.ID = val.str
		case ":timestamp":
			m.Timestamp = atoi64Or(val.sym, 0)
		case ":parent":
			if !val.isNil() {
				m.Parent = val.str
			}
		case ":metadata":
			m.Metadata = parseMetadata(val)
		case ":hash":
			m.Hash = val.str
		case ":tree-root":
			m.TreeRoot = val.str
		case ":tree-entries":
			m.TreeEntries = parseEntries(val)
		case ":upperdir-archive":
			if !val.isNil() {
				m.UpperdirArchive = val.str
			}
		}
	}
	return m, nil
}

func parseMetadata(v sexpr) [][2]string {
	if !v.isList {
		return nil
	}
	var out [][2]string
	for i := 0; i+1 < len(v.list); i += 2 {
		key := strings.TrimPrefix(v.list[i].sym, ":")
		out = append(out, [2]string{key, v.list[i+1].str})
	}
	return out
}

func parseEntries(v sexpr) []Entry {
	if !v.isList {
		return nil
	}
	var out []Entry
	for _, item := range v.list {
		if !item.isList || len(item.list) == 0 || item.list[0].sym != ":file" {
			continue
		}
		e := Entry{}
		fields := item.list[1:]
		for i := 0; i+1 < len(fields); i += 2 {
			switch fields[i].sym {
			case ":path": // tolerate an explicit :path key
				e.Path = fields[i+1].str
			case ":hash":
				e.Hash = fields[i+1].str
			case ":mode":
				e.Mode = atoi64Or(fields[i+1].sym, 0)
			case ":size":
				e.Size = atoi64Or(fields[i+1].sym, 0)
			case ":mtime":
				e.Mtime = atoi64Or(fields[i+1].sym, 0)
			}
		}
		// The path is the first positional string after :file.
		if e.Path == "" && len(fields) > 0 && fields[0].isStr {
			e.Path = fields[0].str
		}
		out = append(out, e)
	}
	return out
}

func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}

func atoi64Or(s string, d int64) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return d
}

// --- minimal s-expression parser -----------------------------------------

type sexpr struct {
	isList bool
	isStr  bool
	list   []sexpr
	str    string // string contents (when isStr)
	sym    string // atom text: symbol, keyword, number, or "nil"
}

func (s sexpr) isNil() bool { return !s.isList && !s.isStr && s.sym == "nil" }

type sexprParser struct {
	src []rune
	pos int
}

func (p *sexprParser) skipSpace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
			continue
		}
		if c == ';' { // line comment
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *sexprParser) parseValue() (sexpr, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return sexpr{}, fmt.Errorf("unexpected end of input")
	}
	c := p.src[p.pos]
	switch {
	case c == '(':
		return p.parseList()
	case c == '"':
		return p.parseString()
	case c == ')':
		return sexpr{}, fmt.Errorf("unexpected )")
	default:
		return p.parseAtom(), nil
	}
}

func (p *sexprParser) parseList() (sexpr, error) {
	p.pos++ // consume (
	var items []sexpr
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return sexpr{}, fmt.Errorf("unterminated list")
		}
		if p.src[p.pos] == ')' {
			p.pos++
			break
		}
		v, err := p.parseValue()
		if err != nil {
			return sexpr{}, err
		}
		items = append(items, v)
	}
	return sexpr{isList: true, list: items}, nil
}

func (p *sexprParser) parseString() (sexpr, error) {
	// Consume a Go/Lisp-style quoted string; strconv.Unquote handles escapes.
	start := p.pos
	p.pos++ // opening quote
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\\' {
			p.pos += 2
			continue
		}
		if c == '"' {
			p.pos++
			raw := string(p.src[start:p.pos])
			s, err := strconv.Unquote(raw)
			if err != nil {
				// Fall back to stripping the outer quotes.
				s = string(p.src[start+1 : p.pos-1])
			}
			return sexpr{isStr: true, str: s}, nil
		}
		p.pos++
	}
	return sexpr{}, fmt.Errorf("unterminated string")
}

func (p *sexprParser) parseAtom() sexpr {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(' || c == ')' || c == '"' {
			break
		}
		p.pos++
	}
	return sexpr{sym: string(p.src[start:p.pos])}
}
