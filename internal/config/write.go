package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/source"
	"github.com/laat/laatmux/internal/tmux"
	"gopkg.in/yaml.v3"
)

// NewRepo is the entry a repository source the list does not have gets
// appended to it, which is what the task form and add take a pasted
// source as: the source in one of the forge forms, git@host:owner/repo,
// ssh:// or https://, as source.Forge reads them, and the name the
// derivation gives it among the listed ones, or, when the source alone
// would rename one of them or get no label, a name of its own (see
// appended). A credential in the source, the user and token of an
// https URL or the password of an ssh one, is left out (Uncredentialed),
// so it never reaches the config, a pending file or a host: the entry's
// source differs from src then. A source that is no forge form, or one
// the list has in any form source.Same takes as one, is an error.
func (c Config) NewRepo(src string) (Repo, error) {
	if _, _, ok := source.Forge(src); !ok {
		return Repo{}, fmt.Errorf("%q is not a repository's source: git@host:owner/repo, ssh://git@host/owner/repo or https://host/owner/repo", src)
	}
	src = Uncredentialed(src)
	if r, ok := c.RepoBySource(src); ok {
		return Repo{}, fmt.Errorf("%s is listed as %s", src, r.Name)
	}
	return appended(c.Repos, src, "")
}

// Uncredentialed is a URL source without the credential it carries: an
// http or https URL without its user and password, which source.Key
// does not count either, and an ssh one without the password after its
// user. Any other source is as it is.
func Uncredentialed(src string) string {
	if !strings.Contains(src, "://") {
		return src
	}
	u, err := url.Parse(src)
	if err != nil || u.User == nil {
		return src
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		u.User = nil
	default:
		if _, set := u.User.Password(); !set {
			return src
		}
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// appended is the entry src gets appended to repos, whose names are
// derived: under name, or, when name is "", the name the derivation
// gives it among them. The entry is the source alone when that gets it
// the name and leaves every other entry's as it was; else it is
// explicit, which renames nothing, since an explicit name takes no part
// in the others' derivation. A name made for it then is the first free
// label of its last path element, its org and that, and that with a
// hash of the source, a name that is no label made one as a checkout's
// is, next_js of next.js.
func appended(repos []Repo, src, name string) (Repo, error) {
	plain := append(slices.Clone(repos), Repo{Source: src})
	if deriveNames(plain) == nil && kept(repos, plain) && (name == "" || plain[len(repos)].Name == name) {
		return plain[len(repos)], nil
	}
	if name == "" {
		taken := map[string]bool{}
		for _, r := range repos {
			taken[r.Name] = true
		}
		org, base := sourceParts(src)
		for _, n := range []string{asLabel(base), asLabel(org + "-" + base), asLabel(base) + "-" + sourceHash(src)} {
			if ValidLabel(n) && !taken[n] {
				name = n
				break
			}
		}
	}
	r := Repo{Source: src, Name: name, Explicit: true}
	list := append(slices.Clone(repos), r)
	if err := deriveNames(list); err != nil {
		return Repo{}, err
	}
	if !kept(repos, list) {
		return Repo{}, fmt.Errorf("repos: %s as %s would rename another repository", src, name)
	}
	return r, nil
}

// kept reports whether the entries of repos keep their names in list,
// the same entries with more after them.
func kept(repos, list []Repo) bool {
	for i := range repos {
		if list[i].Name != repos[i].Name {
			return false
		}
	}
	return true
}

// asLabel is s with every byte a label does not take made _, and a
// leading - too.
func asLabel(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || c == '-') || i == 0 && c == '-' {
			b[i] = '_'
		}
	}
	return string(b)
}

// errChanged is a config file that changed between AddRepo's read and
// its rename: the edit is made again on what is there now.
var errChanged = errors.New("the config file changed while it was written")

// AddRepo appends src to repos in the config file at path, under the
// name given, and reports whether it did: a source the list has, in any
// form source.Same takes as one, is not added again. The entry is the
// source alone when that derives the name and leaves every other
// entry's name as it was, else a mapping with the name (appended).
//
// The file keeps its other content, its comments and its layout: the
// entry is a line put after the list's last item, or a list made under
// a repos: with nothing after it or at the end of the file. A file the
// line cannot be put in, a list written [a, b] say, is written again
// from its yaml.v3 nodes, which keeps the content and the comments but
// not the layout. Either way the result must parse as the file's own
// content with the entry added before it is written. The write goes
// through a temporary renamed over the file, the link's target when
// path is a symlink so the link stays, with the file's mode; a link
// whose target is not there has it made, its directory too. Appends on
// this machine, the daemon's relay and add in the foreground, take
// turns under a lock file in the state directory; a file changed by
// another writer meanwhile, an editor say, is read again and the edit
// made on what it has. A file of more than one YAML document is
// refused: the parsed config is the first alone, and a rewrite would
// drop the rest. Errors name the file as tmux.Printable shows it.
func AddRepo(path, src, name string) (bool, error) {
	real, err := linkTarget(path)
	if err != nil {
		return false, tmux.PrintablePath(err)
	}
	unlock, err := lockAppends()
	if err != nil {
		return false, err
	}
	defer unlock()
	for range 3 {
		added, err := addRepo(real, src, name)
		if !errors.Is(err, errChanged) {
			return added, err
		}
	}
	return false, fmt.Errorf("%s: %w", tmux.Printable(real), errChanged)
}

// linkTarget is the file path names, the links on the way followed,
// whether or not it is there: EvalSymlinks's answer for one that is,
// else the last link's target, as a write through the link makes it.
func linkTarget(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return real, err
	}
	p := path
	for range 40 {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = target
	}
	return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ELOOP}
}

// lockAppends takes the lock AddRepo's appends on this machine take
// turns under: an exclusive flock on config.lock in the state
// directory, a file of its own since the rename replaces the config's
// inode, and kept out of the config's directory, which may be a
// dotfiles checkout.
func lockAppends() (func(), error) {
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return nil, tmux.PrintablePath(err)
	}
	f, err := os.OpenFile(filepath.Join(home.Dir(), "config.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, tmux.PrintablePath(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// addRepo is one try of AddRepo on the file at real.
func addRepo(real, src, name string) (bool, error) {
	// The file as it was before the read: a change after it is seen
	// at the rename.
	before, _ := os.Stat(real)
	b, err := os.ReadFile(real)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, tmux.PrintablePath(err)
	}
	cfg, err := Parse(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	}
	if _, ok := cfg.RepoBySource(src); ok {
		return false, nil
	}
	if n := documents(b); n > 1 {
		return false, fmt.Errorf("%s has %d YAML documents, of which laatmux reads the first; add %s to its repos by hand", tmux.Printable(real), n, src)
	}
	r, err := appended(cfg.Repos, src, name)
	if err != nil {
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	}
	out, ok := insertRepo(b, r)
	if !ok || checkAdded(b, out, r) != nil {
		if out, err = rewriteRepo(b, r); err != nil {
			return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
		}
		if err := checkAdded(b, out, r); err != nil {
			return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
		}
	}
	if err := writeOver(real, out, before); err != nil {
		if errors.Is(err, errChanged) {
			return false, err
		}
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	}
	return true, nil
}

// documents is how many YAML documents b has, as a decoder reads them.
func documents(b []byte) int {
	d := yaml.NewDecoder(bytes.NewReader(b))
	n := 0
	for {
		var node yaml.Node
		if err := d.Decode(&node); err != nil {
			return n
		}
		n++
	}
}

// repoItem is the entry's lines as a list item, without the "- ": the
// source alone, or source and name.
func repoItem(r Repo) []string {
	if !r.Explicit {
		return []string{scalar(r.Source)}
	}
	return []string{"source: " + scalar(r.Source), "name: " + scalar(r.Name)}
}

// repoValue is the entry as yaml decodes it into an any.
func repoValue(r Repo) any {
	if !r.Explicit {
		return r.Source
	}
	return map[string]any{"source": r.Source, "name": r.Name}
}

// scalar is s as one line of YAML, quoted when it would read as
// something else.
func scalar(s string) string {
	b, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// insertRepo puts the entry into the file's text: an item after the
// last item of a block list under repos:, in its indentation; a list
// under a repos: with nothing after it; or a repos: list at the end of a
// file without one. ok is false for any other shape, which rewriteRepo
// takes.
func insertRepo(b []byte, r Repo) ([]byte, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, false
	}
	item := repoItem(r)
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += "\n"
	}
	list := func(indent string) []string {
		out := []string{indent + "- " + item[0] + "\n"}
		for _, l := range item[1:] {
			out = append(out, indent+"  "+l+"\n")
		}
		return out
	}
	at := func(i int, add []string) []byte {
		out := slices.Concat(lines[:i], add, lines[i:])
		return []byte(strings.Join(out, ""))
	}
	if len(doc.Content) == 0 {
		return at(len(lines), append([]string{"repos:\n"}, list("  ")...)), true
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode || top.Style&yaml.FlowStyle != 0 {
		return nil, false
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "repos" {
			continue
		}
		v := top.Content[i+1]
		switch {
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null" && v.Value == "":
			// repos: with nothing after it but a comment: the list
			// goes under it.
			return at(top.Content[i].Line, list("  ")), true
		case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0:
			// After the list's last line with content: the line before
			// the next key's, or the file's end, less the blank and
			// comment lines before it, which belong to what follows.
			end := len(lines)
			if i+2 < len(top.Content) {
				end = top.Content[i+2].Line - 1
			}
			for end > 0 && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#")) {
				end--
			}
			return at(end, list(strings.Repeat(" ", v.Column-1))), true
		}
		return nil, false
	}
	return at(len(lines), append([]string{"repos:\n"}, list("  ")...)), true
}

// rewriteRepo appends the entry to the file's nodes and writes them
// again, with the indentation of the file's first indented line.
func rewriteRepo(b []byte, r Repo) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, HeadComment: doc.HeadComment, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, errors.New("the file is not a mapping")
	}
	var item yaml.Node
	if err := item.Encode(repoValue(r)); err != nil {
		return nil, err
	}
	var seq *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "repos" {
			seq = top.Content[i+1]
		}
	}
	switch {
	case seq == nil:
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "repos"}, seq)
	case seq.Kind == yaml.ScalarNode && seq.Tag == "!!null":
		*seq = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", LineComment: seq.LineComment, HeadComment: seq.HeadComment, FootComment: seq.FootComment}
	case seq.Kind != yaml.SequenceNode:
		return nil, errors.New("repos is not a list")
	}
	seq.Content = append(seq.Content, &item)
	indent := 2
	for _, l := range strings.Split(string(b), "\n") {
		if t := strings.TrimLeft(l, " "); t != "" && t != l && !strings.HasPrefix(t, "#") {
			indent = len(l) - len(t)
			break
		}
	}
	var out bytes.Buffer
	e := yaml.NewEncoder(&out)
	e.SetIndent(indent)
	if err := e.Encode(&doc); err != nil {
		return nil, err
	}
	if err := e.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// checkAdded checks an edit: the result parses as a config, with the
// entry under its name, and decodes as the file did with the entry
// added to repos and nothing else changed.
func checkAdded(old, out []byte, r Repo) error {
	cfg, err := Parse(out)
	if err != nil {
		return err
	}
	if got, ok := cfg.RepoBySource(r.Source); !ok || got.Name != r.Name {
		return fmt.Errorf("repos: %s is not listed as %s after the edit", r.Source, r.Name)
	}
	var want, got any
	if err := yaml.Unmarshal(old, &want); err != nil {
		return err
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		return err
	}
	m, ok := want.(map[string]any)
	if want == nil {
		m, ok = map[string]any{}, true
	}
	if !ok {
		return errors.New("the file is not a mapping")
	}
	list, _ := m["repos"].([]any)
	m["repos"] = append(list, repoValue(r))
	if !reflect.DeepEqual(any(m), got) {
		return errors.New("the edit changed more than repos")
	}
	return nil
}

// writeOver replaces the file at real with data through a temporary
// beside it, with the file's mode, 0644 for a new one; errChanged when
// the file is no longer what before says was read.
func writeOver(real string, data []byte, before fs.FileInfo) error {
	mode := fs.FileMode(0o644)
	if before != nil {
		mode = before.Mode().Perm()
	} else if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return tmux.PrintablePath(err)
	}
	f, err := os.CreateTemp(filepath.Dir(real), filepath.Base(real)+".tmp.*")
	if err != nil {
		return tmux.PrintablePath(err)
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		now, serr := os.Stat(real)
		switch {
		case before == nil && serr == nil, before != nil && (serr != nil || !os.SameFile(before, now) || !now.ModTime().Equal(before.ModTime()) || now.Size() != before.Size()):
			err = errChanged
		}
	}
	if err == nil {
		err = os.Rename(tmp, real)
	}
	if err != nil {
		os.Remove(tmp)
		if errors.Is(err, errChanged) {
			return err
		}
		return tmux.PrintablePath(err)
	}
	return nil
}
