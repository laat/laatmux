package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

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

// testBeforeRename, set by a test, runs in writeOver between the check
// and the rename, to widen the window in which two writers that do not
// take turns would both pass the check; testAfterRead runs in addRepo
// after the read, for a test to change the file as another writer.
var testBeforeRename, testAfterRead func()

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
// else the last link's target, as a write through the link makes it. A
// target's directory is resolved as the system resolves it, a relative
// one from the link's own directory, links before the .. after them,
// which a lexical join would not do: alias/../x through alias, a link
// to real/nested, is real/x.
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
			target = filepath.Dir(p) + string(filepath.Separator) + target
		}
		// The directory unjoined, so EvalSymlinks takes a .. after the
		// link before it.
		i := strings.LastIndexByte(target, filepath.Separator)
		dir, err := resolveDir(target[:max(i, 1)])
		if err != nil {
			return "", err
		}
		p = filepath.Join(dir, target[i+1:])
	}
	return "", &fs.PathError{Op: "readlink", Path: path, Err: syscall.ELOOP}
}

// resolveDir is dir with its links resolved as far as it is there, and
// the part that is not, which the write makes and which has no links,
// after that as it is: alias/../new, with alias a link to real/nested
// and real/new not there yet, is real/new.
func resolveDir(dir string) (string, error) {
	var missing []string
	for d := dir; ; {
		r, err := filepath.EvalSymlinks(d)
		if err == nil {
			return filepath.Join(append([]string{r}, missing...)...), nil
		}
		i := strings.LastIndexByte(d, filepath.Separator)
		if !errors.Is(err, fs.ErrNotExist) || i <= 0 {
			return "", err
		}
		missing = append([]string{d[i+1:]}, missing...)
		d = d[:i]
	}
}

// lockAppends takes the lock AddRepo's appends and SetPaused's edits on
// this machine take turns under: an exclusive flock on config.lock in the state
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
	// Bounded: an append is a moment's work, and a holder stopped in it,
	// a foreground add suspended say, must not hold the daemon's settle
	// for good.
	for deadline := time.Now().Add(lockWait); ; {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("another laatmux has been appending to the config for %s", lockWait)
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// lockWait is how long AddRepo waits for another append on this
// machine to end.
const lockWait = 10 * time.Second

// addRepo is one try of AddRepo on the file at real.
func addRepo(real, src, name string) (bool, error) {
	// The file as it was before the read: a change after it is seen
	// at the rename.
	before, _ := os.Stat(real)
	b, err := os.ReadFile(real)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, tmux.PrintablePath(err)
	}
	if testAfterRead != nil {
		testAfterRead()
	}
	cfg, err := Parse(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	}
	if _, ok := cfg.RepoBySource(src); ok {
		return false, nil
	}
	if n, err := documents(b); err != nil {
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	} else if n > 1 {
		return false, fmt.Errorf("%s has %d YAML documents, of which laatmux reads the first; add %s to its repos by hand", tmux.Printable(real), n, src)
	}
	r, err := appended(cfg.Repos, src, name)
	if err != nil {
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	}
	out, ok := insertRepo(b, r)
	if !ok || checkAdded(b, out, r) != nil {
		if laterDocument(b) {
			// The rewrite is of the first document alone, and would drop
			// the marker of an empty one after it and what it holds.
			return false, fmt.Errorf("%s has a document marker after its first document, which a rewrite of the file would drop; add %s to its repos by hand", tmux.Printable(real), src)
		}
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

// documents is how many YAML documents with content b has, as a
// decoder reads them: an empty one, a --- at the end or one of comments
// alone, is not counted. A document after the first that does not parse
// is an error, since Parse reads the first alone.
func documents(b []byte) (int, error) {
	d := yaml.NewDecoder(bytes.NewReader(b))
	n := 0
	for i := 1; ; i++ {
		var node yaml.Node
		err := d.Decode(&node)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("document %d: %w", i, err)
		}
		if len(node.Content) == 1 && node.Content[0].Kind == yaml.ScalarNode && node.Content[0].Tag == "!!null" && node.Content[0].Value == "" {
			continue
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
	// A list made goes at the first document's end, before the marker
	// of an empty one after it.
	docEnd := firstDocEnd(lines)
	if len(doc.Content) == 0 {
		return at(docEnd, append([]string{"repos:\n"}, list("  ")...)), true
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
			// the next key's, or the document's end, a marker or the
			// file's end, less the blank and comment lines before it,
			// which belong to what follows.
			end := docEnd
			if i+2 < len(top.Content) {
				end = min(end, top.Content[i+2].Line-1)
			}
			for end > 0 && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#")) {
				end--
			}
			return at(end, list(strings.Repeat(" ", v.Column-1))), true
		}
		return nil, false
	}
	return at(docEnd, append([]string{"repos:\n"}, list("  ")...)), true
}

// firstDocEnd is the index of the first document marker, --- or ...,
// after the first document begins, len(lines) when there is none: one
// --- before any content starts the first.
func firstDocEnd(lines []string) int {
	content := false
	for i, l := range lines {
		t := strings.TrimRight(l, "\r\n")
		marker := isMarker(t)
		switch {
		case marker && content:
			return i
		case marker:
			content = true
		case strings.TrimSpace(t) != "" && !strings.HasPrefix(strings.TrimSpace(t), "#"):
			content = true
		}
	}
	return len(lines)
}

// isMarker reports whether a line is a document marker: --- or ...,
// alone or before a space or a tab.
func isMarker(line string) bool {
	for _, m := range []string{"---", "..."} {
		if rest, ok := strings.CutPrefix(line, m); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			return true
		}
	}
	return false
}

// laterDocument reports whether b has a document marker after its first
// document begins (firstDocEnd).
func laterDocument(b []byte) bool {
	lines := strings.Split(string(b), "\n")
	return firstDocEnd(lines) < len(lines)
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
	return encodeLike(b, &doc)
}

// encodeLike writes the document's nodes, with the indentation of the
// first indented line of b, the file they were read from.
func encodeLike(b []byte, doc *yaml.Node) ([]byte, error) {
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
	if err := e.Encode(doc); err != nil {
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
	if err == nil && testBeforeRename != nil {
		testBeforeRename()
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

// SetPaused sets paused on the named host's entry in the config file at
// path, or takes it off, and reports whether it changed the file: an
// entry that is so already is left as it is. The file keeps its other
// content, its comments and its layout, as AddRepo's does: paused: true
// is a line put after the entry's last line with content, in the
// indentation of its keys, or a paused: false line of the entry's own
// said again; resuming takes a paused line of the entry's own out. An
// entry the line cannot be put in or taken out of, one written {name:
// vm, ssh: vm} say, or with paused on its first line, is written again
// from the file's yaml.v3 nodes, which keeps the content and the
// comments but not the layout. Either way the result must parse as the
// file's own content with the entry's paused set or gone before it is
// written, through writeOver, under AddRepo's lock, the file read again
// when another writer changed it meanwhile. A host the file does not
// list is an error, and so is this machine's entry, which nothing
// dials. Errors name the file as tmux.Printable shows it.
func SetPaused(path, host string, paused bool) (bool, error) {
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
		changed, err := setPaused(real, host, paused)
		if !errors.Is(err, errChanged) {
			return changed, err
		}
	}
	return false, fmt.Errorf("%s: %w", tmux.Printable(real), errChanged)
}

// setPaused is one try of SetPaused on the file at real.
func setPaused(real, host string, paused bool) (bool, error) {
	before, _ := os.Stat(real)
	b, err := os.ReadFile(real)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, tmux.PrintablePath(err)
	}
	if testAfterRead != nil {
		testAfterRead()
	}
	cfg, err := Parse(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	}
	// The hosts as Parse read them are the file's hosts list item by
	// item; a file without one has the lone local host, refused here.
	i := slices.IndexFunc(cfg.Hosts, func(h Host) bool { return h.Name == host })
	switch {
	case i < 0:
		names := make([]string, len(cfg.Hosts))
		for j, h := range cfg.Hosts {
			names[j] = h.Name
		}
		return false, fmt.Errorf("unknown host %q; configured: %s", host, strings.Join(names, ", "))
	case cfg.Hosts[i].Local():
		return false, fmt.Errorf("%s is this machine, which nothing dials; only a host reached over ssh is paused", host)
	case cfg.Hosts[i].Paused == paused:
		return false, nil
	}
	if n, err := documents(b); err != nil {
		return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
	} else if n > 1 {
		return false, fmt.Errorf("%s has %d YAML documents, of which laatmux reads the first; set paused on %s by hand", tmux.Printable(real), n, host)
	}
	out, ok := editPaused(b, i, paused)
	if !ok || checkPaused(b, out, i, paused) != nil {
		if laterDocument(b) {
			return false, fmt.Errorf("%s has a document marker after its first document, which a rewrite of the file would drop; set paused on %s by hand", tmux.Printable(real), host)
		}
		if out, err = rewritePaused(b, i, paused); err != nil {
			return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
		}
		if err := checkPaused(b, out, i, paused); err != nil {
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

// hostsEntry is the i-th item of the file's hosts list, with the list
// and the line of the key after hosts, 0 for none; nil when the file
// has no such item.
func hostsEntry(doc *yaml.Node, i int) (seq, entry *yaml.Node, next int) {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, 0
	}
	top := doc.Content[0]
	for k := 0; k+1 < len(top.Content); k += 2 {
		if top.Content[k].Value != "hosts" {
			continue
		}
		seq = top.Content[k+1]
		if k+2 < len(top.Content) {
			next = top.Content[k+2].Line
		}
		if seq.Kind != yaml.SequenceNode || i >= len(seq.Content) {
			return nil, nil, 0
		}
		return seq, seq.Content[i], next
	}
	return nil, nil, 0
}

// editPaused makes the edit in the file's text, on the host's entry, the
// i-th item of a block list under hosts:, itself a block mapping. To
// pause: a paused line of the entry's own, not its first, made paused:
// true, or else paused: true put after the entry's last line with
// content, which is the line before the next item's, or before the key
// after hosts, or the document's end, less the blank and comment lines
// before it, in the indentation of the entry's keys. To resume: the
// paused line of the entry's own taken out. A comment on the line is
// kept. ok is false for any other shape, which rewritePaused takes; so
// is resuming an entry whose paused comes from a mapping merged into it.
func editPaused(b []byte, i int, paused bool) ([]byte, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, false
	}
	seq, entry, next := hostsEntry(&doc, i)
	if entry == nil || doc.Content[0].Style&yaml.FlowStyle != 0 || seq.Style&yaml.FlowStyle != 0 ||
		entry.Kind != yaml.MappingNode || entry.Style&yaml.FlowStyle != 0 || len(entry.Content) == 0 {
		return nil, false
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += "\n"
	}
	indent := strings.Repeat(" ", entry.Column-1)
	eol := "\n"
	if strings.Contains(string(b), "\r\n") {
		eol = "\r\n"
	}
	var key, value *yaml.Node
	for k := 0; k+1 < len(entry.Content); k += 2 {
		if entry.Content[k].Value == "paused" {
			key, value = entry.Content[k], entry.Content[k+1]
		}
	}
	switch {
	case key != nil:
		// A line of its own: not the item's first, which has its -,
		// with the value on it.
		if key == entry.Content[0] || value.Line != key.Line || key.Column != entry.Column {
			return nil, false
		}
		// The line's comment stays: after the value made true, or on a
		// line of its own where the key is taken out.
		comment := value.LineComment
		if comment == "" {
			comment = key.LineComment
		}
		var line []string
		switch {
		case paused && comment != "":
			line = []string{indent + "paused: true " + comment + eol}
		case paused:
			line = []string{indent + "paused: true" + eol}
		case comment != "":
			line = []string{indent + comment + eol}
		}
		at := key.Line - 1
		return []byte(strings.Join(slices.Concat(lines[:at], line, lines[at+1:]), "")), true
	case !paused:
		return nil, false
	}
	end := firstDocEnd(lines)
	switch {
	case i+1 < len(seq.Content):
		end = min(end, seq.Content[i+1].Line-1)
	case next > 0:
		end = min(end, next-1)
	}
	for end > 0 && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#")) {
		end--
	}
	return []byte(strings.Join(slices.Concat(lines[:end], []string{indent + "paused: true" + eol}, lines[end:]), "")), true
}

// rewritePaused sets paused: true on the host's entry in the file's
// nodes, or takes paused out, and writes them again. An entry resumed
// whose paused is not its own, but a mapping's merged into it with <<,
// gets paused: false, which the other entries that merge it do not.
func rewritePaused(b []byte, i int, paused bool) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	_, entry, _ := hostsEntry(&doc, i)
	if entry == nil || entry.Kind != yaml.MappingNode {
		return nil, errors.New("hosts is not a list of mappings")
	}
	yes := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}
	found := false
	for k := 0; k+1 < len(entry.Content); k += 2 {
		if entry.Content[k].Value != "paused" {
			continue
		}
		found = true
		if paused {
			entry.Content[k+1] = yes
		} else {
			entry.Content = slices.Delete(entry.Content, k, k+2)
		}
		break
	}
	if !found {
		v := yes
		if !paused {
			v = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}
		}
		entry.Content = append(entry.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "paused"}, v)
	}
	return encodeLike(b, &doc)
}

// checkPaused checks an edit: the result parses as a config with the
// host's entry paused or not as asked, and decodes as the file did with
// paused set on that entry, or gone from it, or false on it where the
// entry resumed has it so, and nothing else changed.
func checkPaused(old, out []byte, i int, paused bool) error {
	cfg, err := Parse(out)
	if err != nil {
		return err
	}
	if i >= len(cfg.Hosts) || cfg.Hosts[i].Paused != paused {
		return errors.New("hosts: the entry's paused is not as asked after the edit")
	}
	var want, got any
	if err := yaml.Unmarshal(old, &want); err != nil {
		return err
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		return err
	}
	m, _ := want.(map[string]any)
	hosts, _ := m["hosts"].([]any)
	if i >= len(hosts) {
		return errors.New("hosts: the entry is not in the file")
	}
	e, ok := hosts[i].(map[string]any)
	if !ok {
		return errors.New("hosts: the entry is not a mapping")
	}
	gm, _ := got.(map[string]any)
	gh, _ := gm["hosts"].([]any)
	var ge map[string]any
	if i < len(gh) {
		ge, _ = gh[i].(map[string]any)
	}
	switch {
	case paused:
		e["paused"] = true
	case ge["paused"] == false:
		e["paused"] = false
	default:
		delete(e, "paused")
	}
	if !reflect.DeepEqual(want, got) {
		return errors.New("the edit changed more than the host's paused")
	}
	return nil
}
