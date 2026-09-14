// Package rpcports finds RPC listeners reachable from off-host. It reads the
// kernel's socket tables directly, so no ss or netstat is needed.
package rpcports

import (
	"bufio"
	"encoding/hex"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// Ports are the official Monad RPC ports (8080/8081) plus commonly exposed
// EVM RPC ports.
var Ports = []int{8080, 8081, 8545, 8546, 9545, 9546, 18545, 18546}

// procNetTCP and procNetTCP6 are swapped by tests.
var (
	procNetTCP  = "/proc/net/tcp"
	procNetTCP6 = "/proc/net/tcp6"
)

// Exposed returns the RPC ports with a listener on a non-loopback address,
// in Ports order. err is set when the socket tables cannot be read at all,
// in which case the question could not be answered.
func Exposed() ([]int, error) {
	f4, err := os.Open(procNetTCP)
	if err != nil {
		return nil, err
	}
	defer f4.Close()
	var r6 io.Reader
	if f6, err := os.Open(procNetTCP6); err == nil {
		defer f6.Close()
		r6 = f6
	}
	return exposedFrom(f4, r6), nil
}

func exposedFrom(tcp4, tcp6 io.Reader) []int {
	listening := map[int]bool{}
	scan := func(r io.Reader) {
		if r == nil {
			return
		}
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 4 || f[3] != "0A" { // 0A = LISTEN
				continue
			}
			ip, port, ok := parseLocal(f[1])
			if !ok || ip.IsLoopback() {
				continue
			}
			listening[port] = true
		}
	}
	scan(tcp4)
	scan(tcp6)
	var out []int
	for _, p := range Ports {
		if listening[p] {
			out = append(out, p)
		}
	}
	return out
}

// parseLocal decodes the "HEXADDR:HEXPORT" column. Each 32-bit word of the
// address is printed in host byte order, so bytes are reversed per word.
func parseLocal(s string) (net.IP, int, bool) {
	addr, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, false
	}
	port64, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(addr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, false
	}
	ip := make(net.IP, len(raw))
	for w := 0; w < len(raw); w += 4 {
		ip[w], ip[w+1], ip[w+2], ip[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
	}
	return ip, int(port64), true
}
