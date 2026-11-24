// Package imagerrun runs the Talos imager inside a build Job without letting
// the node's machine config reach the Job's log.
//
// The imager prints the whole build profile to stderr before it starts, and
// with an embedded config that profile holds the config: every key and token
// of the node. A container's output is kept by the kubelet and served by
// kubectl logs, so it must not get there. The imager image has no shell to
// redirect with, so the manager binary is copied into the pod (Stage) and
// runs the imager as a child (Run): the child's output goes to memory, never
// to the container's own output. On success only a summary is printed; on
// failure the last lines are printed with every line that holds a piece of
// the config removed.
package imagerrun

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tailLines is how much of a failed build's output is kept to print.
const tailLines = 200

// minSecretLine is the shortest config line that is matched when redacting.
// Shorter ones (a key alone, "---") say nothing about a secret, and matching
// them would hide most of a failure's output.
const minSecretLine = 6

// Run runs argv, keeps its output, and writes a summary to out. It returns
// the exit code to exit with. configPath is the embedded config: its lines
// are what the printed tail must not contain.
func Run(ctx context.Context, argv []string, configPath string, out io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(out, "imager-run: no command to run")
		return 2
	}
	secret, err := os.ReadFile(configPath)
	if err != nil {
		// Without the config the redaction cannot work, and the build could
		// not embed it either.
		fmt.Fprintf(out, "imager-run: read the config to embed: %v\n", err)
		return 2
	}
	tail := newTail(tailLines)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = tail, tail
	fmt.Fprintf(out, "imager-run: running %s (its output is withheld: it contains the machine config)\n", filepath.Base(argv[0]))
	err = cmd.Run()
	if err == nil {
		fmt.Fprintln(out, "imager-run: the build succeeded")
		return 0
	}
	code := 1
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		code = exit.ExitCode()
	}
	fmt.Fprintf(out, "imager-run: the build failed: %v. Its last output, with every line that holds part of the machine config removed:\n", err)
	for _, line := range Redact(tail.lines(), secret) {
		fmt.Fprintln(out, line)
	}
	return code
}

// Redact replaces each line that contains a line of the secret (or the
// profile key that carries it) with a marker.
func Redact(lines []string, secret []byte) []string {
	var needles []string
	sc := bufio.NewScanner(bytes.NewReader(secret))
	sc.Buffer(make([]byte, 0, 64*1024), len(secret)+1)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); len(l) >= minSecretLine {
			needles = append(needles, l)
		}
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if hides(l, needles) {
			out = append(out, "[line withheld: it holds part of the machine config]")
			continue
		}
		out = append(out, l)
	}
	return out
}

func hides(line string, needles []string) bool {
	if strings.Contains(line, "embeddedMachineConfiguration") {
		return true
	}
	for _, n := range needles {
		if strings.Contains(line, n) {
			return true
		}
	}
	return false
}

// Stage copies the running executable to dest, so that a container built
// from another image can run it.
func Stage(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o555)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

// tail keeps the last n lines written to it.
type tail struct {
	n       int
	partial []byte
	buf     []string
}

func newTail(n int) *tail { return &tail{n: n} }

func (t *tail) Write(p []byte) (int, error) {
	t.partial = append(t.partial, p...)
	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		t.push(string(t.partial[:i]))
		t.partial = t.partial[i+1:]
	}
	// One endless line (a progress bar redrawn with \r) must not grow
	// without bound.
	if len(t.partial) > 64*1024 {
		t.push(string(t.partial))
		t.partial = nil
	}
	return len(p), nil
}

func (t *tail) push(line string) {
	t.buf = append(t.buf, line)
	if len(t.buf) > t.n {
		t.buf = t.buf[len(t.buf)-t.n:]
	}
}

func (t *tail) lines() []string {
	if len(t.partial) > 0 {
		t.push(string(t.partial))
		t.partial = nil
	}
	return t.buf
}
