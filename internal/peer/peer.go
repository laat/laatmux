// Package peer names a configured host, a machine laatmux talks to: the
// label it shows under, the ssh alias that reaches it, and the laatmux
// binary there.
package peer

// Host is one machine laatmux talks to. The config inlines it, so the
// field names are the keys of a hosts: entry: name, ssh and bin.
type Host struct {
	Name string // label shown in the sidebar
	SSH  string // ssh alias; "" means this machine
	Bin  string // remote laatmux binary, default "laatmux"
}

// Local reports whether the host is this machine.
func (h Host) Local() bool { return h.SSH == "" }
