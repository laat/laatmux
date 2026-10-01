package protocol

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The wire format, pinned: every field of the envelope and of each
// record set to a value that is not its zero, so no omitempty hides a
// key, marshalled and compared with a golden file. Both ends of every
// other test share the struct, so a renamed JSON tag would pass them;
// this one fails, and a tag renamed on purpose is a golden rewritten
// with -update and a protocol note. A Go field renamed with its tag
// kept, or two fields swapped, changes nothing on the wire and nothing
// here: a string's value is its key, a number's is one, and the golden
// is compared as decoded JSON, so the keys' order does not count. Out
// of reach by the same token: two fields of one type swapping tags,
// since a value made from the key moves with it, and omitempty added
// or dropped, which a Go peer decodes the same.
func TestWireGolden(t *testing.T) {
	for _, c := range []struct {
		name string
		v    any
	}{
		{"message", &Message{}},
		{"agent", &Agent{}}, {"worktree", &Worktree{}}, {"git_status", &GitStatus{}},
		{"pane", &Pane{}}, {"run", &Run{}}, {"attention", &Attention{}},
		{"branch_status", &BranchStatus{}}, {"pending", &Pending{}}, {"handoff", &Handoff{}},
		{"host_status", &HostStatus{}}, {"session", &Session{}}, {"repo_entry", &RepoEntry{}},
		{"listing", &Listing{}}, {"identity", &Identity{}},
	} {
		fill(t, c.name, reflect.ValueOf(c.v).Elem(), nil)
		b, err := json.MarshalIndent(c.v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		// The claim above, checked: no leaf is a zero.
		for _, z := range zeroLeaves(c.name, decoded) {
			t.Errorf("%s: a zero on the wire, which omitempty would hide", z)
		}
		golden(t, c.name, string(b)+"\n", decoded)
	}
}

// The wire values, pinned: every exported constant of the package, the
// message types, capabilities, states, stages, error strings, delivery
// and activity values and the version, as one sorted line each. Both
// ends share the Go name, so a changed value passes every other test
// while a laptop and a host on different builds disagree.
func TestWireConstants(t *testing.T) {
	// A -trimpath test binary has no GOROOT for the importer; go env
	// has it.
	if build.Default.GOROOT == "" {
		if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
			build.Default.GOROOT = strings.TrimSpace(string(out))
		}
	}
	bp, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check("protocol", fset, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, name := range pkg.Scope().Names() {
		if c, ok := pkg.Scope().Lookup(name).(*types.Const); ok && c.Exported() {
			lines = append(lines, name+" = "+c.Val().ExactString())
		}
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"
	p := filepath.Join("testdata", "constants.txt")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v; run with -update", err)
	}
	if string(want) != got {
		t.Errorf("the constants differ from their golden (a changed wire value? run with -update to accept):\n%s", diffSets(string(want), got))
	}
}

// fill sets every field of a struct to a value that is not its zero,
// named for the wire where it can be, so the golden reads as a key
// list: strings get their JSON key, numbers one, bools true, times a
// fixed instant, slices one element, pointers a filled struct, maps one
// entry. Recursion stops at a struct type already on the path, which no
// record has today. A kind it cannot fill fails the test by its path,
// so a field of a new kind cannot slip in with its key unpinned.
func fill(t *testing.T, path string, v reflect.Value, onPath []reflect.Type) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)))
			return
		}
		for _, seen := range onPath {
			if seen == v.Type() {
				return
			}
		}
		onPath = append(onPath, v.Type())
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			sf := v.Type().Field(i)
			name := path + "." + sf.Name
			if !f.CanSet() {
				t.Fatalf("%s: unexported, not on the wire", name)
			}
			if f.Kind() == reflect.String {
				key := strings.Split(sf.Tag.Get("json"), ",")[0]
				if key == "" {
					key = sf.Name
				}
				f.SetString(key)
				continue
			}
			fill(t, name, f, onPath)
		}
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fill(t, path, v.Elem(), onPath)
	case reflect.Slice:
		e := reflect.New(v.Type().Elem()).Elem()
		fill(t, path+"[]", e, onPath)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), e))
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fill(t, path+"[]", v.Index(i), onPath)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fill(t, path+"{key}", k, onPath)
		e := reflect.New(v.Type().Elem()).Elem()
		fill(t, path+"{}", e, onPath)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("x")
	default:
		t.Fatalf("%s: a %s, which fill cannot set; teach it the kind", path, v.Kind())
	}
}

// zeroLeaves is the paths of the zero values in a decoded JSON value:
// "", 0, false, null, an empty array or object, or the zero time.
func zeroLeaves(path string, v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			return []string{path + " = {}"}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, zeroLeaves(path+"."+k, x[k])...)
		}
	case []any:
		if len(x) == 0 {
			return []string{path + " = []"}
		}
		for i, e := range x {
			out = append(out, zeroLeaves(fmt.Sprintf("%s[%d]", path, i), e)...)
		}
	case string:
		if x == "" || x == "0001-01-01T00:00:00Z" {
			out = append(out, fmt.Sprintf("%s = %q", path, x))
		}
	case float64:
		if x == 0 {
			out = append(out, path+" = 0")
		}
	case bool:
		if !x {
			out = append(out, path+" = false")
		}
	case nil:
		out = append(out, path+" = null")
	}
	return out
}

// diffSets is the lines only in want, marked -, and only in got,
// marked +: both are sorted and unique, so an added or removed line
// shows alone, not as the rest of the file shifted.
func diffSets(want, got string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(want), "\n") {
		w[l] = true
	}
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		g[l] = true
	}
	var out []string
	for l := range w {
		if !g[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range g {
		if !w[l] {
			out = append(out, "+ "+l)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// golden compares the decoded value with the golden file's, decoded
// too, and names the paths that differ.
func golden(t *testing.T, name, got string, decoded any) {
	t.Helper()
	p := filepath.Join("testdata", name+".json")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v; run with -update", err)
	}
	var wantv any
	if err := json.Unmarshal(want, &wantv); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	if diffs := diffJSON(name, wantv, decoded); len(diffs) > 0 {
		t.Errorf("%s differs from its golden (a renamed key? run with -update to accept):\n%s", name, strings.Join(diffs, "\n"))
	}
}

// diffJSON is the paths at which two decoded JSON values differ, keys
// in any order.
func diffJSON(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want an object, got %v", path, got)}
		}
		var out []string
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			wv, inW := w[k]
			gv, inG := g[k]
			switch {
			case !inW:
				out = append(out, fmt.Sprintf("%s.%s: not in the golden", path, k))
			case !inG:
				out = append(out, fmt.Sprintf("%s.%s: in the golden, not on the wire", path, k))
			default:
				out = append(out, diffJSON(path+"."+k, wv, gv)...)
			}
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{fmt.Sprintf("%s: want %v, got %v", path, want, got)}
		}
		var out []string
		for i := range w {
			out = append(out, diffJSON(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return out
	default:
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s: want %v, got %v", path, want, got)}
		}
		return nil
	}
}
