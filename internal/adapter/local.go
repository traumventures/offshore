package adapter

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"
)

// runLocalCommand runs a local tool with the terminal attached and
// returns its exit code like a shell would. When stderr is not a terminal,
// lines starting with one of drop are removed from it.
func runLocalCommand(dir string, env []string, tool string, args []string, drop ...string) int {
	bin, err := RealBinary(tool)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	cmd := exec.Command(bin, args...)
	cmd.Args[0] = tool
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	var f *lineFilter
	if len(drop) > 0 && !term.IsTerminal(int(os.Stderr.Fd())) {
		f = newLineFilter(os.Stderr, drop...)
		cmd.Stderr = f
	}
	code := runChild(cmd)
	if f != nil {
		f.Flush()
	}
	return code
}

// runChild runs cmd in the foreground: terminal signals reach the child
// (same process group), the proxy ignores them while it waits. The exit
// code follows the shell convention (128+N for signal N).
func runChild(cmd *exec.Cmd) int {
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Reset(syscall.SIGINT, syscall.SIGQUIT)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "offshore: %v\n", err)
	return 126
}

// --- output filtering ---

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// lineFilter writes through to w, dropping lines (terminated by \n or \r)
// whose text without ANSI escapes and leading blanks starts with one of
// the prefixes. Status lines of the build phase that a normal run would
// not print are removed this way.
type lineFilter struct {
	w    io.Writer
	drop []string
	buf  []byte
}

func newLineFilter(w io.Writer, drop ...string) *lineFilter { return &lineFilter{w: w, drop: drop} }

func (f *lineFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexAny(f.buf, "\r\n")
		if i < 0 {
			return len(p), nil
		}
		line := f.buf[:i+1]
		if !f.dropped(line) {
			if _, err := f.w.Write(line); err != nil {
				return len(p), err
			}
		}
		f.buf = f.buf[i+1:]
	}
}

func (f *lineFilter) dropped(line []byte) bool {
	s := strings.TrimLeft(ansiRE.ReplaceAllString(string(line), ""), " \t\r")
	for _, d := range f.drop {
		if strings.HasPrefix(s, d) {
			return true
		}
	}
	return false
}

// Flush writes a pending partial line.
func (f *lineFilter) Flush() {
	if len(f.buf) > 0 && !f.dropped(f.buf) {
		f.w.Write(f.buf)
	}
	f.buf = nil
}

// sniffer passes writes through and remembers whether a line contained
// one of the needles (e.g. a tool's "command not found" message).
type sniffer struct {
	w       io.Writer
	needles []string
	mu      sync.Mutex
	tail    []byte
	hit     string
}

func newSniffer(w io.Writer, needles ...string) *sniffer { return &sniffer{w: w, needles: needles} }

func (s *sniffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	buf := append(s.tail, p...)
	if s.hit == "" {
		plain := ansiRE.ReplaceAll(buf, nil)
		for _, n := range s.needles {
			if bytes.Contains(plain, []byte(n)) {
				s.hit = n
			}
		}
	}
	if len(buf) > 512 {
		buf = buf[len(buf)-512:]
	}
	s.tail = append([]byte{}, buf...)
	s.mu.Unlock()
	return s.w.Write(p)
}

// found returns the needle that was seen, or "".
func (s *sniffer) found() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hit
}

// colorTerminal reports whether stderr is a color terminal.
func colorTerminal() bool {
	return term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("NO_COLOR") == ""
}
