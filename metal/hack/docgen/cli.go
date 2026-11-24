package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// helpFunc returns what `pk-metal <args>` prints on stdout. The tests feed it
// recorded help text; main runs the binary.
type helpFunc func(args ...string) (string, error)

// execHelp runs the binary with an empty environment apart from PATH, so that
// no default in the output can come from the machine docgen runs on.
func execHelp(bin string) helpFunc {
	return func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("pk-metal %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
}

// cliIndex is the top-level help: the commands in the order help lists them,
// the global flags and the exit status.
type cliIndex struct {
	Program     string       `json:"program"`
	Description string       `json:"description"`
	Usage       string       `json:"usage"`
	Commands    []cliSummary `json:"commands"`
	GlobalFlags []cliFlag    `json:"globalFlags"`
	ExitStatus  []exitCode   `json:"exitStatus"`
}

type cliSummary struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Changes is true for a command that changes a machine or what the
	// operator does (help lists those apart).
	Changes bool `json:"changes"`
}

// cliCommand is one verb: what `pk-metal <name> -h` prints, split into parts.
type cliCommand struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	Synopsis string `json:"synopsis"`
	// Usage is every usage line, with the program name.
	Usage   []string `json:"usage"`
	Changes bool     `json:"changes"`
	// NeedsCluster is true when the command reads or writes the cluster: it
	// takes --kubeconfig and --context.
	NeedsCluster bool `json:"needsCluster"`
	// Description is the free text of the help, verbatim (pre-formatted):
	// everything except the summary, usage, examples, exit status and flags.
	Description string          `json:"description"`
	Examples    []string        `json:"examples"`
	ExitStatus  []exitCode      `json:"exitStatus"`
	Flags       []cliFlag       `json:"flags"`
	Subcommands []cliSubcommand `json:"subcommands"`
}

type cliSubcommand struct {
	Name     string    `json:"name"`
	Synopsis string    `json:"synopsis"`
	Flags    []cliFlag `json:"flags"`
}

type cliFlag struct {
	// Name is without dashes; the site writes it as --name.
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default"`
	Usage   string `json:"usage"`
	// Global is true for --kubeconfig and --context, which every cluster
	// command shares and the global flags page describes once.
	Global bool `json:"global"`
}

type exitCode struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

var globalFlagNames = map[string]bool{"kubeconfig": true, "context": true}

func readCLI(help helpFunc) (cliIndex, []cliCommand, error) {
	top, err := help("help")
	if err != nil {
		return cliIndex{}, nil, err
	}
	index, err := parseIndex(top)
	if err != nil {
		return cliIndex{}, nil, err
	}
	var cmds []cliCommand
	for _, s := range index.Commands {
		out, err := help(s.Name, "-h")
		if err != nil {
			return cliIndex{}, nil, err
		}
		c, err := parseCommand(s, out)
		if err != nil {
			return cliIndex{}, nil, fmt.Errorf("%s: %w", s.Name, err)
		}
		for i, sub := range c.Subcommands {
			out, err := help(c.Name, sub.Name, "-h")
			if err != nil {
				return cliIndex{}, nil, err
			}
			flags, err := parseFlagsSection(out)
			if err != nil {
				return cliIndex{}, nil, fmt.Errorf("%s %s: %w", c.Name, sub.Name, err)
			}
			c.Subcommands[i].Flags = flags
			c.NeedsCluster = c.NeedsCluster || hasGlobal(flags)
		}
		cmds = append(cmds, c)
	}
	return index, cmds, nil
}

var (
	commandLine = regexp.MustCompile(`^  (\S+)\s+(.+)$`)
	globalLine  = regexp.MustCompile(`^  --(\S+) (\S+)\s+(.+)$`)
	exitLine    = regexp.MustCompile(`^\s+(\d+)\s+(.+)$`)
	flagLine    = regexp.MustCompile(`^  -(\S+)(?: (.+))?$`)
	defaultTail = regexp.MustCompile(`\s*\(default (.*)\)$`)
	headerLine  = regexp.MustCompile(`^[a-z][a-z ]*:$`)
	bareWord    = regexp.MustCompile(`^[a-z][a-z-]*$`)
)

// parseIndex reads `pk-metal help`.
func parseIndex(text string) (cliIndex, error) {
	idx := cliIndex{Program: "pk-metal", Commands: []cliSummary{}, GlobalFlags: []cliFlag{}, ExitStatus: []exitCode{}}
	var desc []string
	section := "description"
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "usage: "):
			idx.Usage = strings.TrimPrefix(line, "usage: ")
			section = ""
			continue
		case strings.HasPrefix(line, "commands that change no machine"):
			section = "readonly"
			continue
		case strings.HasPrefix(line, "commands that change"):
			section = "changes"
			continue
		case strings.HasPrefix(line, "global flags"):
			section = "global"
			continue
		case line == "exit status:":
			section = "exit"
			continue
		case headerLine.MatchString(line):
			section = ""
			continue
		}
		switch section {
		case "description":
			desc = append(desc, line)
		case "readonly", "changes":
			if m := commandLine.FindStringSubmatch(line); m != nil {
				idx.Commands = append(idx.Commands, cliSummary{Name: m[1], Summary: m[2], Changes: section == "changes"})
			}
		case "global":
			if m := globalLine.FindStringSubmatch(line); m != nil {
				idx.GlobalFlags = append(idx.GlobalFlags, cliFlag{Name: m[1], Type: m[2], Usage: m[3], Global: true})
			}
		case "exit":
			idx.ExitStatus = appendExit(idx.ExitStatus, line)
		}
	}
	idx.Description = strings.TrimPrefix(joinWrapped(desc), "pk-metal: ")
	if len(idx.Commands) == 0 {
		return idx, errors.New("help lists no commands")
	}
	return idx, nil
}

// parseCommand reads `pk-metal <name> -h`: a summary line, the usage line(s),
// free text with optional "examples:" and "exit status:" blocks, and the
// flag.PrintDefaults block after "flags:".
func parseCommand(s cliSummary, text string) (cliCommand, error) {
	c := cliCommand{
		Name: s.Name, Summary: s.Summary, Changes: s.Changes,
		Usage: []string{}, Examples: []string{}, ExitStatus: []exitCode{}, Flags: []cliFlag{}, Subcommands: []cliSubcommand{},
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	prefix := "pk-metal " + s.Name + ": "
	if len(lines) == 0 || !strings.HasPrefix(lines[0], prefix) {
		return c, fmt.Errorf("help does not start with %q", prefix)
	}
	i := 1
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || !strings.HasPrefix(lines[i], "usage: ") {
		return c, errors.New("help has no usage line")
	}
	c.Usage = append(c.Usage, strings.TrimSpace(strings.TrimPrefix(lines[i], "usage: ")))
	for i++; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "pk-metal "); i++ {
		c.Usage = append(c.Usage, strings.TrimSpace(lines[i]))
	}
	c.Synopsis = strings.TrimSpace(strings.TrimPrefix(c.Usage[0], "pk-metal "+s.Name))

	var desc []string
	section := "description"
	for ; i < len(lines); i++ {
		line := lines[i]
		if line == "flags:" {
			break
		}
		switch {
		case line == "examples:":
			section = "examples"
			continue
		case line == "exit status:":
			section = "exit"
			continue
		case section != "description" && strings.TrimSpace(line) == "":
			section = "description"
		}
		switch section {
		case "examples":
			c.Examples = append(c.Examples, strings.TrimSpace(line))
		case "exit":
			c.ExitStatus = appendExit(c.ExitStatus, line)
		default:
			desc = append(desc, line)
		}
	}
	c.Description = strings.Trim(strings.Join(desc, "\n"), "\n")

	flags, err := parseFlagsSection(text)
	if err != nil {
		return c, err
	}
	c.Flags = flags
	c.NeedsCluster = hasGlobal(flags)
	c.Subcommands = subcommands(c.Synopsis, s.Name)
	return c, nil
}

// subcommands reads "list | show <rollout> | approve [--yes] <rollout>": a
// synopsis of alternatives that each start with a plain word. A synopsis like
// "[-n namespace] <claim> | <namespace>/<claim>" has none.
func subcommands(synopsis, verb string) []cliSubcommand {
	alts := strings.Split(synopsis, " | ")
	if len(alts) < 2 {
		return []cliSubcommand{}
	}
	subs := make([]cliSubcommand, 0, len(alts))
	for _, alt := range alts {
		alt = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(alt), "pk-metal "+verb))
		word, _, _ := strings.Cut(alt, " ")
		if !bareWord.MatchString(word) {
			return []cliSubcommand{}
		}
		subs = append(subs, cliSubcommand{Name: word, Synopsis: alt, Flags: []cliFlag{}})
	}
	return subs
}

// parseFlagsSection reads the flag.PrintDefaults output after "flags:": a
// line "  -name type" (no type for a bool) and the usage on lines indented by
// four spaces and a tab, with " (default x)" at its end when the default is
// not the zero value.
func parseFlagsSection(text string) ([]cliFlag, error) {
	flags := []cliFlag{}
	_, block, found := strings.Cut(text, "\nflags:\n")
	if !found {
		return flags, nil
	}
	var cur *cliFlag
	finish := func() {
		if cur == nil {
			return
		}
		cur.Usage = strings.TrimSpace(cur.Usage)
		if m := defaultTail.FindStringSubmatch(cur.Usage); m != nil {
			cur.Usage = strings.TrimSpace(cur.Usage[:len(cur.Usage)-len(m[0])])
			cur.Default = m[1]
			if u, err := strconv.Unquote(m[1]); err == nil {
				cur.Default = u
			}
		}
		flags = append(flags, *cur)
		cur = nil
	}
	for line := range strings.SplitSeq(block, "\n") {
		if m := flagLine.FindStringSubmatch(line); m != nil {
			finish()
			typ := m[2]
			if typ == "" {
				typ = "bool"
			}
			cur = &cliFlag{Name: m[1], Type: typ, Global: globalFlagNames[m[1]]}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if cur == nil || !strings.HasPrefix(line, "    \t") {
			return nil, fmt.Errorf("unexpected line in flags: %q", line)
		}
		cur.Usage += " " + strings.TrimSpace(line)
	}
	finish()
	return flags, nil
}

// appendExit adds "  2  text" as a new code, or an indented continuation line
// to the last one.
func appendExit(codes []exitCode, line string) []exitCode {
	if m := exitLine.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[1])
		return append(codes, exitCode{Code: n, Meaning: strings.TrimSpace(m[2])})
	}
	if t := strings.TrimSpace(line); t != "" && len(codes) > 0 {
		codes[len(codes)-1].Meaning += " " + t
	}
	return codes
}

// joinWrapped turns hard-wrapped lines into paragraphs separated by one blank line.
func joinWrapped(lines []string) string {
	var paras []string
	cur := ""
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			if cur != "" {
				paras = append(paras, cur)
				cur = ""
			}
			continue
		}
		if cur == "" {
			cur = t
		} else {
			cur += " " + t
		}
	}
	if cur != "" {
		paras = append(paras, cur)
	}
	return strings.Join(paras, "\n\n")
}

func hasGlobal(flags []cliFlag) bool {
	for _, f := range flags {
		if f.Global {
			return true
		}
	}
	return false
}
