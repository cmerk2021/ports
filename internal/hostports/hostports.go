// Package hostports lists listening TCP and bound UDP sockets on the host by
// reading Linux /proc/net/{tcp,tcp6,udp,udp6}. These files are world-readable
// and do not require privileges, but they do not reveal socket ownership
// (that would need /proc/<pid>/fd access), so every listener reported here
// has an unknown owner.
package hostports

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cmerk2021/ports/internal/model"
)

// Listener is one listening TCP or bound UDP socket.
type Listener struct {
	Protocol    string `json:"protocol"`
	BindAddress string `json:"bind_address"`
	Port        int    `json:"port"`
}

// Snapshot is the result of a host inspection.
type Snapshot struct {
	Listeners []Listener `json:"listeners"`
	// Unavailable lists sources that could not be read (e.g. no /proc).
	Unavailable []string `json:"unavailable,omitempty"`
}

// Available reports whether at least one source could be read.
func (s *Snapshot) Available() bool { return len(s.Unavailable) < 4 }

const tcpListen = "0A"

// Scan reads the four /proc/net socket tables under dir.
func Scan(dir string) *Snapshot {
	snap := &Snapshot{}
	seen := map[Listener]bool{}
	for _, src := range []struct{ file, proto string }{
		{"tcp", model.ProtoTCP}, {"tcp6", model.ProtoTCP}, {"udp", model.ProtoUDP}, {"udp6", model.ProtoUDP},
	} {
		f, err := os.Open(filepath.Join(dir, src.file))
		if err != nil {
			reason := err.Error()
			if errors.Is(err, fs.ErrNotExist) {
				reason = "not present"
			}
			snap.Unavailable = append(snap.Unavailable, fmt.Sprintf("%s: %s", filepath.Join(dir, src.file), reason))
			continue
		}
		ls, err := parseTable(f, src.proto)
		f.Close()
		if err != nil {
			snap.Unavailable = append(snap.Unavailable, fmt.Sprintf("%s: %v", filepath.Join(dir, src.file), err))
			continue
		}
		for _, l := range ls {
			if !seen[l] {
				seen[l] = true
				snap.Listeners = append(snap.Listeners, l)
			}
		}
	}
	sort.Slice(snap.Listeners, func(i, j int) bool {
		a, b := snap.Listeners[i], snap.Listeners[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		return a.BindAddress < b.BindAddress
	})
	return snap
}

func parseTable(r io.Reader, proto string) ([]Listener, error) {
	var out []Listener
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		state := fields[3]
		if proto == model.ProtoTCP && state != tcpListen {
			continue
		}
		if proto == model.ProtoUDP {
			// Only unconnected (bound) UDP sockets occupy a port for binding
			// purposes; state 07 is TCP_CLOSE which UDP uses for "unconnected".
			if state != "07" {
				continue
			}
		}
		addr, port, err := parseAddr(fields[1])
		if err != nil {
			return nil, err
		}
		if port == 0 {
			continue
		}
		out = append(out, Listener{Protocol: proto, BindAddress: addr, Port: port})
	}
	return out, sc.Err()
}

// parseAddr decodes "0100007F:1F90" style addresses. The address is stored as
// host-order 32-bit words (little-endian on all Linux targets we support).
func parseAddr(s string) (string, int, error) {
	hexIP, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, fmt.Errorf("malformed address %q", s)
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return "", 0, fmt.Errorf("malformed port in %q", s)
	}
	raw, err := hex.DecodeString(hexIP)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", 0, fmt.Errorf("malformed IP in %q", s)
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	ip, _ := netip.AddrFromSlice(raw)
	return ip.Unmap().String(), int(port), nil
}

// Matching returns listeners that may contend with a socket on port/proto/bind.
func (s *Snapshot) Matching(port int, proto, bind string) []Listener {
	var out []Listener
	for _, l := range s.Listeners {
		if l.Port == port && model.ProtocolsOverlap(l.Protocol, proto) && model.BindsOverlap(l.BindAddress, bind) {
			out = append(out, l)
		}
	}
	return out
}
