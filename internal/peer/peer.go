// Package peer names a configured host, a machine laatmux talks to: the
// label it shows under, the ssh alias that reaches it, and the laatmux
// binary there.
package peer

// Host is one machine laatmux talks to. The config inlines it, so the
// field names are the keys of a hosts: entry: name, ssh, bin and paused.
type Host struct {
	Name string // label shown in the sidebar
	SSH  string // ssh alias; "" means this machine
	Bin  string // remote laatmux binary, default "laatmux"
	// Paused is that this machine does not dial the host: nothing that
	// runs on its own does, and a command aimed at it is refused with a
	// *PausedError, upgrade aside. Only a host reached over ssh is.
	Paused bool
}

// Local reports whether the host is this machine.
func (h Host) Local() bool { return h.SSH == "" }

// PausedError is the refusal of a command aimed at a paused host.
type PausedError struct{ Name string }

func (e *PausedError) Error() string {
	return "host " + e.Name + " is paused; laatmux hosts resume " + e.Name + " connects it"
}
