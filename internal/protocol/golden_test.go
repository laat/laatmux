package protocol

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The wire format, pinned: every field of the envelope and of each
// record set to a value that is not its zero, so no omitempty hides a
// key, marshalled and compared with a golden file. Both ends of every
// other test share the struct, so a renamed JSON tag would pass them;
// this one fails, and a tag renamed on purpose is a golden rewritten
// with -update and a protocol note.
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
		fill(reflect.ValueOf(c.v).Elem(), 0)
		b, err := json.MarshalIndent(c.v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		golden(t, c.name, string(b)+"\n")
	}
}

// fill sets every field of a struct to a value that is not its zero,
// named after the field where it can be, so the golden reads as a key
// list: strings get their field name, numbers their index from one,
// bools true, times a fixed instant, slices one element, pointers a
// filled struct, maps one entry. Nested structs stop three levels down.
func fill(v reflect.Value, depth int) {
	if depth > 3 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if !f.CanSet() {
				continue
			}
			switch f.Kind() {
			case reflect.String:
				f.SetString(v.Type().Field(i).Name)
			case reflect.Int, reflect.Int64:
				f.SetInt(int64(i + 1))
			case reflect.Uint64:
				f.SetUint(uint64(i + 1))
			case reflect.Bool:
				f.SetBool(true)
			default:
				fill(f, depth+1)
			}
		}
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fill(v.Elem(), depth)
	case reflect.Slice:
		e := reflect.New(v.Type().Elem()).Elem()
		if e.Kind() == reflect.String {
			e.SetString("x")
		} else {
			fill(e, depth+1)
		}
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), e))
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fill(v.Index(i), depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		if k.Kind() == reflect.String {
			k.SetString("k")
		}
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint64:
		v.SetUint(1)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("x")
	}
}

func golden(t *testing.T, name, got string) {
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
	if string(want) != got {
		t.Errorf("%s differs from its golden (a renamed key? run with -update to accept):\n%s", name, got)
	}
}
