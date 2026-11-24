package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// command is one verb: what it does in a plain line, how to call it, and
// whether it can change a machine or the cluster.
type command struct {
	name     string
	synopsis string // arguments after "pk-metal <name>"
	summary  string // one plain line: what it does
	changes  bool   // true: it changes a machine or what the operator does
	run      func(args []string)
}

// commands is the verb table: main dispatches on it and help prints it, so
// the two cannot disagree.
var commands []command

func init() {
	commands = []command{
		{name: "status", synopsis: "[-o table|json]", run: statusCmd,
			summary: "show machines, BMCs, claims and operations, with the reason when something is stuck (reads only)"},
		{name: "top", synopsis: "[--fleet url] [--workload-kubeconfig file]", run: topCmd, changes: true,
			summary: "live view of every machine: BMC, phase, operation, node; keys approve, hold and retry like the commands below"},
		{name: "approve", synopsis: "[-n namespace] [--yes] [<operation> | <namespace>/<operation>]", run: approveCmd, changes: true,
			summary: "show what a proposed operation will do to a machine, then let it run; without a name, list the operations that wait for approval"},
		{name: "retry", synopsis: "[-n namespace] [--yes] <operation> | <namespace>/<operation>", run: retryCmd, changes: true,
			summary: "delete a failed operation, so that the operator proposes it again (it then waits for approval again)"},
		{name: "abort", synopsis: "[-n namespace] [--reason text] <operation> | <namespace>/<operation>", run: abortCmd, changes: true,
			summary: "stop an operation before its next step and undo what it left behind (media, boot override, cordon)"},
		{name: "reboot", synopsis: "[--mode default|powercycle] [--drain-timeout d] [--grace-period s] [--delete-emptydir-data] <node>", run: rebootCmd, changes: true,
			summary: "propose a clean reboot of a Talos node: cordon, drain, reboot, wait until ready, uncordon (waits for approval)"},
		{name: "hold", synopsis: "[-n namespace] [--reason text] <claim> | <namespace>/<claim>", run: holdCmd, changes: true,
			summary: "stop the operator from proposing new operations for a claim; nothing on the machine changes"},
		{name: "unhold", synopsis: "[-n namespace] <claim> | <namespace>/<claim>", run: unholdCmd, changes: true,
			summary: "remove a hold, so that the operator proposes operations for the claim again"},
		{name: "pivot", synopsis: "--to <kubeconfig> [--to-context name] [--yes] [flags]", run: pivotCmd, changes: true,
			summary: "move the operator and its objects from this cluster to another one, for example from kind onto the Talos cluster it built"},
		{name: "check", synopsis: "[flags] <bmc-url>", run: checkCmd,
			summary: "test that a BMC answers and list its virtual media and systems (reads only)"},
		{name: "inventory", synopsis: "[flags] <bmc-url>", run: inventoryCmd,
			summary: "read the hardware inventory from a BMC and print it as YAML (reads only)"},
		{name: "render", synopsis: "--dir <manifests> --secrets <secrets.yaml|-> [flags]", run: renderCmd,
			summary: "write Talos machine configs, talosconfig and kubeconfig from TalosCluster and TalosNode files (touches no machine)"},
		{name: "boot", synopsis: "--config <rendered.yaml> --node <name> (--listen addr | --relay addr) [flags] <bmc-url>", run: bootCmd, changes: true,
			summary: "mount a machine config ISO through a BMC and REBOOT the machine into it, without the operator"},
		{name: "bootstrap", synopsis: "--dir <manifests> --secrets <secrets.yaml|-> [flags]", run: bootstrapCmd, changes: true,
			summary: "start a new Talos cluster on booted machines and wait until every node is ready, without the operator"},
		{name: "relay", synopsis: "[--front addr] [--tunnel addr] [--advertise host:port] [--secret-file file]", run: relayCmd,
			summary: "run on a host that can reach the BMCs: BMCs download boot media from it, and it forwards that traffic to pk-metal boot"},
		{name: "sim", synopsis: "serve [flags] | fleet --config <file>", run: simCmd,
			summary: "run simulated BMCs for tests: serve one, or a fleet with a virtual machine behind each"},
		{name: "version", run: versionCmd,
			summary: "print the version of this program"},
	}
}

func versionCmd(args []string) {
	fs := newFlags("version")
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		badUsage(fs, "version takes no arguments")
	}
	fmt.Println(version)
}

func lookup(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func main() {
	args, err := parseGlobal(os.Args[1:])
	if err != nil {
		die(err)
	}
	if len(args) == 0 {
		usage(os.Stderr)
		os.Exit(1)
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		if len(args) > 1 {
			c := lookup(args[1])
			if c == nil {
				dief("unknown command %q; run: pk-metal help", args[1])
			}
			c.run([]string{"-h"})
			return
		}
		usage(os.Stdout)
		return
	}
	c := lookup(args[0])
	if c == nil {
		dief("unknown command %q; run: pk-metal help", args[0])
	}
	c.run(args[1:])
}

// global holds the flags every cluster verb shares. They may come before the
// verb (pk-metal --context lab status) or after it (pk-metal status --context lab).
var global struct {
	kubeconfig string
	context    string
}

func addGlobalFlags(fs *flag.FlagSet) {
	fs.StringVar(&global.kubeconfig, "kubeconfig", global.kubeconfig,
		"kubeconfig file (default: $KUBECONFIG, then ~/.kube/config, then the in-cluster account)")
	fs.StringVar(&global.context, "context", global.context, "kubeconfig context to use (default: the current context)")
}

// parseGlobal reads the global flags in front of the verb and returns the rest.
func parseGlobal(args []string) ([]string, error) {
	fs := flag.NewFlagSet("pk-metal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return []string{"help"}, nil
		}
		return nil, fmt.Errorf("%v; run: pk-metal help", err)
	}
	return fs.Args(), nil
}

// newFlags returns the flag set for a verb. Its -h output is the verb's
// summary, its synopsis and its flags.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		w := fs.Output()
		verb := strings.Fields(name)[0]
		if c := lookup(verb); c != nil {
			fmt.Fprintf(w, "pk-metal %s: %s\n\n", c.name, c.summary)
			fmt.Fprintf(w, "usage: pk-metal %s %s\n", c.name, c.synopsis)
			if extra := verbHelp[verb]; extra != "" {
				fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(extra))
			}
		}
		fmt.Fprintln(w, "\nflags:")
		fs.PrintDefaults()
	}
	return fs
}

// verbHelp is longer help text for the verbs that need it, printed by -h.
var verbHelp = map[string]string{}

// parseFlags parses a verb's flags. -h prints the verb's help to stdout and
// exits 0; a bad flag prints the error and the usage to stderr and exits 1.
func parseFlags(fs *flag.FlagSet, args []string) {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "-h" || a == "-help" || a == "--help" {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			os.Exit(0)
		}
	}
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		badUsage(fs, err.Error())
	}
}

// badUsage reports a wrong call of a verb, with its usage line, and exits 1.
func badUsage(fs *flag.FlagSet, msg string) {
	verb := strings.Fields(fs.Name())[0]
	fmt.Fprintf(os.Stderr, "pk-metal: %s\n", msg)
	if c := lookup(verb); c != nil {
		fmt.Fprintf(os.Stderr, "usage: pk-metal %s %s\n", c.name, c.synopsis)
	}
	fmt.Fprintf(os.Stderr, "run: pk-metal help %s\n", verb)
	os.Exit(1)
}

// die prints the error as "pk-metal: <message>" on stderr and exits 1.
func die(err error) {
	fmt.Fprintf(os.Stderr, "pk-metal: %v\n", err)
	os.Exit(1)
}

func dief(format string, args ...any) {
	die(fmt.Errorf(format, args...))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `pk-metal: manage bare-metal machines through their BMCs (Redfish) and the
pk-metal operator that runs in a Kubernetes cluster.

usage: pk-metal [--kubeconfig file] [--context name] <command> [flags] [arguments]

`)
	for _, changes := range []bool{false, true} {
		if changes {
			fmt.Fprintln(w, "\ncommands that change a machine, or what the operator does:")
		} else {
			fmt.Fprintln(w, "commands that change no machine:")
		}
		for _, c := range commands {
			if c.changes == changes {
				fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
			}
		}
	}
	fmt.Fprint(w, `
help:
  pk-metal help <command>     flags and details of one command (or: pk-metal <command> -h)

global flags (for the commands that talk to the cluster):
  --kubeconfig file           kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)
  --context name              kubeconfig context (default: the current context)

exit status:
  0  success
  1  error; the message is on stderr, as "pk-metal: <message>"
  2  pk-metal status only: something is blocked or failed and needs a person
`)
}
