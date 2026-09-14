// Package netinfo validates addresses and ports and detects the host's public
// IPv4 address.
package netinfo

import (
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ipv4Re = regexp.MustCompile(`^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$`)

// ValidIPv4 reports whether s is a dotted-quad IPv4 address with every octet
// in range. Leading zeros are read as decimal.
func ValidIPv4(s string) bool {
	m := ipv4Re.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	for _, o := range m[1:] {
		n, err := strconv.Atoi(o)
		if err != nil || n > 255 {
			return false
		}
	}
	return true
}

var portRe = regexp.MustCompile(`^[0-9]{1,5}$`)

// ValidPort reports whether s is a decimal port in 1..65535.
func ValidPort(s string) bool {
	if !portRe.MatchString(s) {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// DefaultIPURL is the service asked for the public IPv4 address.
const DefaultIPURL = "https://ifconfig.me"

// Client is an HTTP client restricted to IPv4: 10 s to connect, total as
// given.
func Client(total time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: total,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return d.DialContext(ctx, "tcp4", addr)
			},
			Proxy: http.ProxyFromEnvironment,
		},
	}
}

// DetectPublicIPv4 asks url for the address and returns it only if it is a
// valid IPv4 literal. Any failure yields "".
func DetectPublicIPv4(url string) string {
	resp, err := Client(20 * time.Second).Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(b))
	if !ValidIPv4(ip) {
		return ""
	}
	return ip
}
