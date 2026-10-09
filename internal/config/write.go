package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/tmux"
	"gopkg.in/yaml.v3"
)

// errChanged is a config file that changed between an edit's read and
// its rename: the edit is made again on what is there now.
var errChanged = errors.New("the config file changed while it was written")

// testBeforeRename, set by a test, runs in writeOver between the check
// and the rename, to widen the window in which two writers that do not
// take turns would both pass the check; testAfterRead runs in setPaused
// after the read, for a test to change the file as another writer.
var testBeforeRename, testAfterRead func()

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

// lockEdits takes the lock SetPaused's edits on this machine take turns
// under: an exclusive flock on config.lock in the state directory, a
// file of its own since the rename replaces the config's inode, and
// kept out of the config's directory, which may be a dotfiles checkout.
func lockEdits() (func(), error) {
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return nil, tmux.PrintablePath(err)
	}
	f, err := os.OpenFile(filepath.Join(home.Dir(), "config.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, tmux.PrintablePath(err)
	}
	// Bounded: an edit is a moment's work, and a holder stopped in it,
	// a pause suspended say, must not hold the others for good.
	for deadline := time.Now().Add(lockWait); ; {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("another laatmux has been editing the config for %s", lockWait)
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

// lockWait is how long an edit waits for another on this machine to
// end.
const lockWait = 10 * time.Second

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
// content, its comments and its layout: paused: true is a line put
// after the entry's last line with content, in the indentation of its
// keys, or a paused: false line of the entry's own said again;
// resuming takes a paused line of the entry's own out. An entry the
// line cannot be put in or taken out of, one written {name: vm, ssh:
// vm} say, or with paused on its first line, is written again from the
// file's yaml.v3 nodes, which keeps the content and the comments but
// not the layout. Either way the result must parse as the file's own
// content with the entry's paused set or gone before it is written.
// The write goes through a temporary renamed over the file, the link's
// target when path is a symlink so the link stays, with the file's
// mode (writeOver). Edits on this machine take turns under a lock file
// in the state directory; a file changed by another writer meanwhile,
// an editor say, is read again and the edit made on what it has. A
// file of more than one YAML document is refused: the parsed config is
// the first alone, and a rewrite would drop the rest. A host the file
// does not list is an error, and so is this machine's entry, which
// nothing dials. Errors name the file as tmux.Printable shows it.
func SetPaused(path, host string, paused bool) (bool, error) {
	real, err := linkTarget(path)
	if err != nil {
		return false, tmux.PrintablePath(err)
	}
	unlock, err := lockEdits()
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
	// A resume takes paused out, or, where that leaves the entry
	// paused through a mapping merged into it, says paused: false.
	modes := []bool{false}
	if !paused {
		modes = append(modes, true)
	}
	var out []byte
	for _, explicit := range modes {
		if o, ok := editPaused(b, i, paused, explicit); ok && checkPaused(b, o, i, paused) == nil {
			out = o
			break
		}
	}
	if out == nil {
		if laterDocument(b) {
			return false, fmt.Errorf("%s has a document marker after its first document, which a rewrite of the file would drop; set paused on %s by hand", tmux.Printable(real), host)
		}
		for _, explicit := range modes {
			o, err := rewritePaused(b, i, paused, explicit)
			if err == nil {
				err = checkPaused(b, o, i, paused)
			}
			if err == nil {
				out = o
				break
			}
			if explicit == modes[len(modes)-1] {
				return false, fmt.Errorf("%s: %w", tmux.Printable(real), err)
			}
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
// paused line of the entry's own taken out, or, explicit, made paused:
// false, or put in as paused: true is, for an entry paused through a
// mapping merged into it. A comment on the line is kept. ok is false
// for any other shape, which rewritePaused takes.
func editPaused(b []byte, i int, paused, explicit bool) ([]byte, bool) {
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
	eol := "\n"
	if strings.Contains(string(b), "\r\n") {
		eol = "\r\n"
	}
	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += eol
	}
	indent := strings.Repeat(" ", entry.Column-1)
	value := "paused: true"
	if !paused {
		value = "paused: false"
	}
	var key, val *yaml.Node
	for k := 0; k+1 < len(entry.Content); k += 2 {
		if entry.Content[k].Value == "paused" {
			key, val = entry.Content[k], entry.Content[k+1]
		}
	}
	switch {
	case key != nil:
		// A line of its own: not the item's first, which has its -,
		// with the value on it.
		if key == entry.Content[0] || val.Line != key.Line || key.Column != entry.Column {
			return nil, false
		}
		// The line's comment stays: after the value said again, or on a
		// line of its own where the key is taken out.
		comment := val.LineComment
		if comment == "" {
			comment = key.LineComment
		}
		var line []string
		switch {
		case (paused || explicit) && comment != "":
			line = []string{indent + value + " " + comment + eol}
		case paused || explicit:
			line = []string{indent + value + eol}
		case comment != "":
			line = []string{indent + comment + eol}
		}
		at := key.Line - 1
		return []byte(strings.Join(slices.Concat(lines[:at], line, lines[at+1:]), "")), true
	case !paused && !explicit:
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
	return []byte(strings.Join(slices.Concat(lines[:end], []string{indent + value + eol}, lines[end:]), "")), true
}

// rewritePaused sets paused: true on the host's entry in the file's
// nodes, or takes paused out, or, explicit, sets paused: false, for an
// entry paused through a mapping merged into it with <<, which the
// other entries that merge it are not; and writes them again. The
// comments on a value said again stay on it, and those of a key taken
// out go above the entry.
func rewritePaused(b []byte, i int, paused, explicit bool) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	_, entry, _ := hostsEntry(&doc, i)
	if entry == nil || entry.Kind != yaml.MappingNode {
		return nil, errors.New("hosts is not a list of mappings")
	}
	value := "true"
	if !paused {
		value = "false"
	}
	found := false
	for k := 0; k+1 < len(entry.Content); k += 2 {
		if entry.Content[k].Value != "paused" {
			continue
		}
		found = true
		if paused || explicit {
			v := entry.Content[k+1]
			v.Kind, v.Tag, v.Value, v.Style, v.Alias, v.Content = yaml.ScalarNode, "!!bool", value, 0, nil, nil
			break
		}
		var comments []string
		for _, n := range entry.Content[k : k+2] {
			for _, c := range []string{n.HeadComment, n.LineComment, n.FootComment} {
				if c != "" {
					comments = append(comments, c)
				}
			}
		}
		if len(comments) > 0 {
			if entry.HeadComment != "" {
				comments = append([]string{entry.HeadComment}, comments...)
			}
			entry.HeadComment = strings.Join(comments, "\n")
		}
		entry.Content = slices.Delete(entry.Content, k, k+2)
		break
	}
	switch {
	case !found && (paused || explicit):
		entry.Content = append(entry.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "paused"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value})
	case !found:
		return nil, errors.New("hosts: the entry has no paused of its own to take out")
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
