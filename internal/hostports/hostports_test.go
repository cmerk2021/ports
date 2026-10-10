package hostports

import (
	"os"
	"path/filepath"
	"testing"
)

const tcp4 = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
   2: 0100007F:9C40 0100007F:1F90 01 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0
`

const tcp6 = `  sl  local_address                         remote_address                        st
   0: 00000000000000000000000000000000:1FA0 00000000000000000000000000000000:0000 0A 00000000:00000000
   1: 00000000000000000000000001000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000
`

const udp4 = `  sl  local_address rem_address   st
   0: 00000000:0035 00000000:0000 07 00000000:00000000
   1: 0A00000A:0044 0101A8C0:0043 01 00000000:00000000
`

func TestScan(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tcp"), []byte(tcp4), 0o644)
	os.WriteFile(filepath.Join(dir, "tcp6"), []byte(tcp6), 0o644)
	os.WriteFile(filepath.Join(dir, "udp"), []byte(udp4), 0o644)
	snap := Scan(dir)
	if len(snap.Unavailable) != 1 || !snap.Available() {
		t.Fatalf("unavailable = %v", snap.Unavailable)
	}
	want := []Listener{
		{"tcp", "127.0.0.1", 53},
		{"udp", "0.0.0.0", 53},
		{"tcp", "::1", 443},
		{"tcp", "0.0.0.0", 8080},
		{"tcp", "::", 8096},
	}
	if len(snap.Listeners) != len(want) {
		t.Fatalf("got %v", snap.Listeners)
	}
	for i := range want {
		if snap.Listeners[i] != want[i] {
			t.Errorf("listener %d: got %v want %v", i, snap.Listeners[i], want[i])
		}
	}
	if m := snap.Matching(53, "tcp", "0.0.0.0"); len(m) != 1 || m[0].Protocol != "tcp" {
		t.Fatalf("wildcard bind must match specific listener: %v", m)
	}
	if m := snap.Matching(53, "tcp", "10.0.0.1"); len(m) != 0 {
		t.Fatalf("different specific address matched: %v", m)
	}
	if m := snap.Matching(8096, "tcp", "192.168.1.5"); len(m) != 1 {
		t.Fatalf("specific bind must match wildcard listener: %v", m)
	}
}

func TestScanUnavailable(t *testing.T) {
	snap := Scan(filepath.Join(t.TempDir(), "missing"))
	if snap.Available() || len(snap.Listeners) != 0 {
		t.Fatalf("got %+v", snap)
	}
}

func TestScanMalformed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tcp"), []byte("header\n 0: zz:yy 0 0A\n"), 0o644)
	snap := Scan(dir)
	if len(snap.Unavailable) != 4 {
		t.Fatalf("malformed table should be reported unavailable: %v", snap.Unavailable)
	}
}
