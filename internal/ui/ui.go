// Package ui is the terminal surface of monad-failover: coloured output,
// phase banners, prompts and the fatal-error type every other package returns.
//
// Wording is stable: the recovery document and the test suite depend on it.
package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ANSI sequences. They are always emitted; the run log carries them too.
const (
	Bold   = "\033[1m"
	Dim    = "\033[2m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Cyan   = "\033[36m"
	Reset  = "\033[0m"
)

// Fatal is the error every step returns when the run must stop. Msg is the
// headline; Lines are the indented recovery instructions printed under it.
type Fatal struct {
	Msg   string
	Lines []string
}

func (f *Fatal) Error() string { return f.Msg }

// Die builds a Fatal. Callers return it; main prints it and exits 1.
func Die(msg string, lines ...string) error {
	return &Fatal{Msg: msg, Lines: lines}
}

// Console owns the two output streams and the input the prompts read from.
type Console struct {
	Out io.Writer
	Err io.Writer

	in    *bufio.Reader
	inFd  int
	isTTY bool
}

// New wires a console to the given streams. stdin may be nil when no prompt
// will ever be answered (the dry run); a prompt then fails cleanly.
func New(stdout, stderr io.Writer, stdin *os.File) *Console {
	c := &Console{Out: stdout, Err: stderr, inFd: -1}
	if stdin != nil {
		c.in = bufio.NewReader(stdin)
		c.inFd = int(stdin.Fd())
		c.isTTY = isTerminal(c.inFd)
	}
	return c
}

// Tee adds w as a second destination for both streams. Used once the run log
// is open, so everything the operator sees is also on disk.
func (c *Console) Tee(w io.Writer) {
	c.Out = io.MultiWriter(c.Out, w)
	c.Err = io.MultiWriter(c.Err, w)
}

func (c *Console) Printf(format string, a ...any) { fmt.Fprintf(c.Out, format, a...) }
func (c *Console) Println(a ...any)               { fmt.Fprintln(c.Out, a...) }
func (c *Console) Blank()                         { fmt.Fprintln(c.Out) }

// Header prints the boxed banner with hostname and timestamp.
func (c *Console) Header(version string) {
	host, _ := os.Hostname()
	fmt.Fprintln(c.Out)
	fmt.Fprintln(c.Out, "┌───────────────────────────────────────────────────────────")
	fmt.Fprintf(c.Out, "│  %s%-44s%s%12s\n", Bold, "MONAD VALIDATOR FAILOVER", Reset, "v"+version)
	fmt.Fprintf(c.Out, "│  %s · %s\n", host, time.Now().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintln(c.Out, "└───────────────────────────────────────────────────────────")
}

// Phase prints the full-width step banner, e.g. "━━ [4/8] KEY IMPORT ━━━…".
func (c *Console) Phase(n, total int, title string) {
	ascii := fmt.Sprintf("-- [%d/%d] %s ", n, total, title)
	fill := 60 - len(ascii)
	fmt.Fprintln(c.Out)
	fmt.Fprintf(c.Out, "%s━━ [%d/%d] %s ", Cyan, n, total, title)
	if fill > 0 {
		fmt.Fprint(c.Out, strings.Repeat("━", fill))
	}
	fmt.Fprintf(c.Out, "%s\n", Reset)
}

func (c *Console) Bar() {
	fmt.Fprintf(c.Out, "%s──────────────────────────────────────────────%s\n", Dim, Reset)
}

func (c *Console) Step(text string) {
	fmt.Fprintln(c.Out)
	fmt.Fprintf(c.Out, "%s▸%s %s%s%s\n", Cyan, Reset, Bold, text, Reset)
}

func (c *Console) OK(text string)   { fmt.Fprintf(c.Out, "%s✔%s %s\n", Green, Reset, text) }
func (c *Console) Warn(text string) { fmt.Fprintf(c.Out, "%s⚠%s %s\n", Yellow, Reset, text) }

// Cross prints a red ✗ line on stdout; the dry run uses it for failed checks.
func (c *Console) Cross(text string) { fmt.Fprintf(c.Out, "%s✗%s %s\n", Red, Reset, text) }

// Report prints a Fatal (or any error) on the error stream: "✗ headline"
// followed by indented instruction lines.
func (c *Console) Report(err error) {
	var f *Fatal
	if !errors.As(err, &f) {
		f = &Fatal{Msg: err.Error()}
	}
	msg := f.Msg
	if msg == "" {
		msg = "Aborted."
	}
	fmt.Fprintf(c.Err, "%s✗%s %s\n", Red, Reset, msg)
	for _, l := range f.Lines {
		fmt.Fprintf(c.Err, "   %s\n", l)
	}
}

// ErrNoInput is returned when a prompt hits end of input.
var ErrNoInput = errors.New("no input available for prompt")

func (c *Console) readLine() (string, error) {
	if c.in == nil {
		return "", ErrNoInput
	}
	line, err := c.in.ReadString('\n')
	if err != nil {
		if err == io.EOF && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", ErrNoInput
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ConfirmYN prints "  ? prompt (y/N) › " and returns true for y/yes.
// End of input counts as "no".
func (c *Console) ConfirmYN(prompt string) bool {
	fmt.Fprintf(c.Out, "  %s?%s %s (y/N) › ", Cyan, Reset, prompt)
	ans, err := c.readLine()
	if err != nil {
		fmt.Fprintln(c.Out)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y", "yes":
		return true
	}
	return false
}

// Ask prints "  ? label › " and returns the line typed. End of input is an
// error: a required answer cannot be defaulted silently.
func (c *Console) Ask(label string) (string, error) {
	fmt.Fprintf(c.Out, "  %s?%s %s › ", Cyan, Reset, label)
	ans, err := c.readLine()
	if err != nil {
		fmt.Fprintln(c.Out)
		return "", Die("Input ended while waiting for: " + label)
	}
	return ans, nil
}

// AskRaw prints the label verbatim and returns the line typed.
func (c *Console) AskRaw(label string) (string, error) {
	fmt.Fprint(c.Out, label)
	ans, err := c.readLine()
	if err != nil {
		fmt.Fprintln(c.Out)
		return "", Die("Input ended while waiting for: " + strings.TrimSpace(label))
	}
	return ans, nil
}

// AskHidden reads a secret with terminal echo disabled. When stdin is not a
// terminal (a pipe in the test suite) it reads a plain line. The value is
// returned as bytes so the caller can zero it after use.
func (c *Console) AskHidden(label string) ([]byte, error) {
	fmt.Fprintf(c.Out, "  %s?%s %s › ", Cyan, Reset, label)
	restore := func() {}
	if c.isTTY {
		if r, err := disableEcho(c.inFd); err == nil {
			restore = r
		}
	}
	ans, err := c.readLine()
	restore()
	fmt.Fprintln(c.Out)
	if err != nil {
		return nil, Die("Input ended while waiting for: " + label)
	}
	return []byte(ans), nil
}

// ── terminal echo control (linux) ────────────────────────────────────

func isTerminal(fd int) bool {
	if fd < 0 {
		return false
	}
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	return e == 0
}

func disableEcho(fd int) (func(), error) {
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&old))); e != 0 {
		return nil, e
	}
	cur := old
	cur.Lflag &^= syscall.ECHO
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&cur))); e != 0 {
		return nil, e
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&old)))
	}, nil
}

// Zero overwrites a secret buffer. Go cannot promise no copy exists elsewhere
// (SECURITY.md says so plainly); this removes the one we hold.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
