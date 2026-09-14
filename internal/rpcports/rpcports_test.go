package rpcports

import (
	"reflect"
	"strings"
	"testing"
)

const header4 = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
const header6 = "  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// hex helpers: 0.0.0.0 = 00000000, 127.0.0.1 = 0100007F, 203.0.113.7 = 0771 00CB in
// little-endian per word → "077100CB"; ports 8080 = 1F90, 8081 = 1F91, 22 = 0016.
func line4(addr, port, st string) string {
	return "   0: " + addr + ":" + port + " 00000000:0000 " + st + " 00000000:00000000 00:00000000 00000000     0        0 1 0 0\n"
}

func line6(addr, port, st string) string {
	return "   0: " + addr + ":" + port + " 00000000000000000000000000000000:0000 " + st + " 00000000:00000000 00:00000000 00000000     0        0 1 0 0\n"
}

func TestExposedFrom(t *testing.T) {
	cases := []struct {
		name string
		tcp4 string
		tcp6 string
		want []int
	}{
		{"wildcard 8080", header4 + line4("00000000", "1F90", "0A"), "", []int{8080}},
		{"specific public ip 8080", header4 + line4("077100CB", "1F90", "0A"), "", []int{8080}},
		{"loopback 8080", header4 + line4("0100007F", "1F90", "0A"), "", nil},
		{"loopback other 127.x", header4 + line4("0500007F", "1F90", "0A"), "", nil},
		{"ipv6 loopback 8081", "", header6 + line6("00000000000000000000000001000000", "1F91", "0A"), nil},
		{"ipv6 any 8081", "", header6 + line6("00000000000000000000000000000000", "1F91", "0A"), []int{8081}},
		{"ipv4-mapped loopback", "", header6 + line6("0000000000000000FFFF00000100007F", "1F90", "0A"), nil},
		{"ipv4-mapped public", "", header6 + line6("0000000000000000FFFF0000077100CB", "1F90", "0A"), []int{8080}},
		{"established, not listening", header4 + line4("00000000", "1F90", "01"), "", nil},
		{"non-rpc port", header4 + line4("00000000", "0016", "0A"), "", nil},
		{"several, in Ports order", header4 + line4("00000000", "1F91", "0A") + line4("00000000", "1F90", "0A"), "", []int{8080, 8081}},
		{"garbage lines ignored", header4 + "junk\n" + line4("zz", "1F90", "0A"), "", nil},
		{"empty", "", "", nil},
	}
	for _, tc := range cases {
		var r6 *strings.Reader
		if tc.tcp6 != "" {
			r6 = strings.NewReader(tc.tcp6)
		}
		var got []int
		if r6 == nil {
			got = exposedFrom(strings.NewReader(tc.tcp4), nil)
		} else {
			got = exposedFrom(strings.NewReader(tc.tcp4), r6)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestExposedMissingTables(t *testing.T) {
	old := procNetTCP
	procNetTCP = "/nonexistent/tcp"
	defer func() { procNetTCP = old }()
	if _, err := Exposed(); err == nil {
		t.Error("expected an error when the socket table is unreadable")
	}
}
