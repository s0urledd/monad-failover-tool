package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func console(t *testing.T, input string) (*Console, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(input)
	w.Close()
	t.Cleanup(func() { r.Close() })
	var out, errb bytes.Buffer
	return New(&out, &errb, r), &out, &errb
}

func TestPrompts(t *testing.T) {
	c, out, _ := console(t, "y\nYES\nno\n\nvalue\nsecret\n")
	if !c.ConfirmYN("first?") || !c.ConfirmYN("second?") || c.ConfirmYN("third?") || c.ConfirmYN("fourth?") {
		t.Error("ConfirmYN answers wrong")
	}
	v, err := c.Ask("label")
	if err != nil || v != "value" {
		t.Errorf("Ask: %q %v", v, err)
	}
	s, err := c.AskHidden("secret")
	if err != nil || string(s) != "secret" {
		t.Errorf("AskHidden: %q %v", s, err)
	}
	// end of input: ConfirmYN is "no", Ask is an error
	if c.ConfirmYN("eof?") {
		t.Error("ConfirmYN at EOF answered yes")
	}
	if _, err := c.Ask("eof"); err == nil {
		t.Error("Ask at EOF succeeded")
	}
	plain := strings.NewReplacer(Cyan, "", Reset, "").Replace(out.String())
	if !strings.Contains(plain, "  ? first? (y/N) › ") || !strings.Contains(plain, "  ? label › ") {
		t.Errorf("prompt text:\n%s", plain)
	}
}

func TestAskCRLF(t *testing.T) {
	c, _, _ := console(t, "value\r\n")
	if v, _ := c.Ask("x"); v != "value" {
		t.Errorf("CRLF not stripped: %q", v)
	}
}

func TestReport(t *testing.T) {
	c, _, errb := console(t, "")
	c.Report(Die("headline", "line one", "line two"))
	want := Red + "✗" + Reset + " headline\n   line one\n   line two\n"
	if errb.String() != want {
		t.Errorf("got %q want %q", errb.String(), want)
	}
	errb.Reset()
	c.Report(Die(""))
	if !strings.Contains(errb.String(), "Aborted.") {
		t.Errorf("empty message: %q", errb.String())
	}
}

func TestPhaseWidth(t *testing.T) {
	c, out, _ := console(t, "")
	c.Phase(4, 8, "VALIDATOR KEY IMPORT")
	line := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(out.String(), Cyan, ""), Reset, ""))
	// the banner is padded to 60 display columns ("━━ " counts as 3)
	if n := len([]rune(line)); n != 60 {
		t.Errorf("banner width %d: %q", n, line)
	}
}

func TestZero(t *testing.T) {
	b := []byte("secret")
	Zero(b)
	if string(b) != "\x00\x00\x00\x00\x00\x00" {
		t.Error("not zeroed")
	}
}
