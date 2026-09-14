// Package uptime asks the monval validator API (operated by Huginn) how the
// network sees this validator after cutover. It is advisory: every failure
// prints the URL to check later and the run continues.
package uptime

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/s0urledd/monad-failover-tool/internal/netinfo"
)

// Report is the subset of the response the tool shows. Empty strings mean
// the field was missing or null.
type Report struct {
	Name          string
	Status        string
	UptimePercent string
	Finalized     string
	Timeout       string
	LastRound     string
}

// URL builds the per-validator endpoint.
func URL(base, secpPub string) string { return base + "/" + secpPub }

// Fetch returns the report and true, or false when the API is unreachable,
// returned an error, or did not report success.
func Fetch(url string) (Report, bool) {
	resp, err := netinfo.Client(30 * time.Second).Get(url)
	if err != nil {
		return Report{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Report{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return Report{}, false
	}
	return Parse(body)
}

// Parse reads the response body. Fields are looked up at the top level and
// inside the "uptime" object; null and absent are both "".
func Parse(body []byte) (Report, bool) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return Report{}, false
	}
	if strings.TrimSpace(string(top["success"])) != "true" {
		return Report{}, false
	}
	fields := map[string]json.RawMessage{}
	for k, v := range top {
		fields[k] = v
	}
	if raw, ok := top["uptime"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) == nil {
			for k, v := range inner {
				fields[k] = v
			}
		}
	}
	get := func(k string) string {
		r, ok := fields[k]
		if !ok {
			return ""
		}
		s := strings.TrimSpace(string(r))
		if s == "" || s == "null" {
			return ""
		}
		if strings.HasPrefix(s, `"`) {
			var v string
			if json.Unmarshal(r, &v) != nil {
				return ""
			}
			return v
		}
		if s == "true" || s == "false" || strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			return ""
		}
		return s
	}
	return Report{
		Name:          get("validator_name"),
		Status:        get("status"),
		UptimePercent: get("uptime_percent"),
		Finalized:     get("finalized_count"),
		Timeout:       get("timeout_count"),
		LastRound:     get("last_round"),
	}, true
}
