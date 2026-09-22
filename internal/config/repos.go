package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Repo is one known repository. Identity is the source; the name is a
// label used only to place new clones, worktrees and session names, and it
// is derived from the source unless set explicitly.
type Repo struct {
	Source string
	Name   string
	// Explicit is set when the name came from the file rather than from
	// derivation.
	Explicit bool
}

// A list entry is either a source string or a mapping with source and an
// optional name.
func (r *Repo) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Source = n.Value
		return nil
	}
	var m struct {
		Source string `yaml:"source"`
		Name   string `yaml:"name"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	r.Source, r.Name, r.Explicit = m.Source, m.Name, m.Name != ""
	return nil
}

func (r Repo) MarshalYAML() (any, error) {
	if !r.Explicit {
		return r.Source, nil
	}
	return map[string]string{"source": r.Source, "name": r.Name}, nil
}

// Repo finds a known repository by name or source.
func (c Config) Repo(nameOrSource string) (Repo, bool) {
	for _, r := range c.Repos {
		if r.Name == nameOrSource || r.Source == nameOrSource {
			return r, true
		}
	}
	return Repo{}, false
}

// deriveNames fills in Name for entries without an explicit one, so that
// every host and every run derive the same name from the same list:
//
//  1. The last path component without .git.
//  2. If two sources share that, each is prefixed with its org, the path
//     component before the name, joined with -.
//  3. If they still collide, or a source has no org component, the name is
//     suffixed with the first six hex digits of the source's SHA-256.
//
// Explicit names win over derivation but must still be unique. Duplicate
// sources and duplicate final names are errors.
func deriveNames(repos []Repo) error {
	bySource := map[string]int{}
	for i, r := range repos {
		if r.Source == "" {
			return fmt.Errorf("repos: entry %d has no source", i+1)
		}
		if j, dup := bySource[r.Source]; dup {
			return fmt.Errorf("repos: %s listed twice (entries %d and %d)", r.Source, j+1, i+1)
		}
		bySource[r.Source] = i
	}
	type parts struct{ org, base string }
	derived := map[int]parts{}
	baseCount := map[string]int{}
	for i, r := range repos {
		if r.Explicit {
			continue
		}
		org, base := sourceParts(r.Source)
		derived[i] = parts{org, base}
		baseCount[base]++
	}
	cand := map[int]string{}
	hashed := map[int]bool{}
	candCount := map[string]int{}
	for i, p := range derived {
		switch {
		case baseCount[p.base] == 1:
			cand[i] = p.base
		case p.org != "":
			cand[i] = p.org + "-" + p.base
		default:
			cand[i] = p.base + "-" + sourceHash(repos[i].Source)
			hashed[i] = true
		}
		candCount[cand[i]]++
	}
	for i := range derived {
		if candCount[cand[i]] > 1 && !hashed[i] {
			cand[i] += "-" + sourceHash(repos[i].Source)
		}
	}
	byName := map[string]int{}
	for i := range repos {
		r := &repos[i]
		if !r.Explicit {
			r.Name = cand[i]
		}
		if !ValidLabel(r.Name) {
			if r.Explicit {
				return fmt.Errorf("repos: name %q for %s is not a valid label (%s)", r.Name, r.Source, labelChars)
			}
			return fmt.Errorf("repos: derived name %q for %s is not a valid label (%s); set an explicit name", r.Name, r.Source, labelChars)
		}
		if j, dup := byName[r.Name]; dup {
			return fmt.Errorf("repos: %s and %s both get the name %s; set an explicit name on one", repos[j].Source, r.Source, r.Name)
		}
		byName[r.Name] = i
	}
	return nil
}

// sourceParts splits a source into its org and name: the last path
// component without .git, and the component before it. Handles URLs
// (https://host/org/name.git), scp-like specs (git@host:org/name.git) and
// local paths.
func sourceParts(src string) (org, name string) {
	s := strings.TrimRight(src, "/")
	var p string
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			p = rest[j+1:]
		}
	} else if i := strings.IndexByte(s, ':'); i >= 0 && !strings.Contains(s[:i], "/") {
		p = s[i+1:]
	} else {
		p = s
	}
	p = strings.TrimSuffix(strings.TrimRight(p, "/"), ".git")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	name = parts[len(parts)-1]
	if len(parts) >= 2 {
		org = parts[len(parts)-2]
	}
	return org, name
}

func sourceHash(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:3])
}
