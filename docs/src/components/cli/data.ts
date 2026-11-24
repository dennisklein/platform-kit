// The pk-metal command data for the CLI reference components.
// Source: src/data/generated/cli/*.json, which metal/hack/docgen writes from the
// command table (metal/cmd/pk-metal/cli.go) and the help of each command.
import { readFileSync } from 'node:fs';
import { root } from '../../../scripts/root.mjs';

const dir = `${root}/src/data/generated/cli`;

export interface Flag {
  name: string;
  type: string;
  default: string;
  usage: string;
  global: boolean;
}
export interface Sub {
  name: string;
  synopsis: string;
  flags: Flag[];
}
export interface Command {
  name: string;
  summary: string;
  synopsis: string;
  usage: string[];
  changes: boolean;
  needsCluster: boolean;
  description: string;
  examples: string[];
  exitStatus: { code: number; meaning: string }[];
  flags: Flag[];
  subcommands: Sub[];
}
export interface Index {
  program: string;
  usage: string;
  commands: { name: string; summary: string; changes: boolean }[];
  globalFlags: Flag[];
  exitStatus: { code: number; meaning: string }[];
}

const read = (name: string) => JSON.parse(readFileSync(`${dir}/${name}.json`, 'utf8'));

export const index: Index = read('index');

const cache = new Map<string, Command>();
export function command(name: string): Command {
  if (!index.commands.some((c) => c.name === name)) {
    throw new Error(`cli: "${name}" is not a pk-metal command (src/data/generated/cli/index.json)`);
  }
  if (!cache.has(name)) cache.set(name, read(name));
  return cache.get(name)!;
}

/**
 * The groups of the command table, by purpose. Each command is in one group.
 * CliIndex stops the build when index.json has a command that no group names.
 */
export const groups = [
  {
    id: 'inspect',
    title: 'Inspect',
    lead: 'Read the state of the machines, the BMCs and the clusters.',
    commands: ['status', 'top', 'check', 'inventory', 'version'],
  },
  {
    id: 'control',
    title: 'Approve and control',
    lead: 'Decide what the operator does with its proposals.',
    commands: ['approve', 'retry', 'abort', 'hold', 'unhold', 'rollout'],
  },
  {
    id: 'day-1',
    title: 'Day 1: first machines',
    lead: 'Start the first machines of a site without the operator, then move the operator.',
    commands: ['render', 'boot', 'bootstrap', 'relay', 'pivot'],
  },
  {
    id: 'day-2',
    title: 'Day 2: operation',
    lead: 'Ask the operator for a change to a node or to a cluster that runs.',
    commands: ['reboot', 'upgrade', 'scale', 'reinstall', 'replace', 'wipe', 'backup', 'restore'],
  },
  {
    id: 'lab',
    title: 'Lab and simulation',
    lead: 'Run simulated BMCs for tests and for the virtual lab.',
    commands: ['sim'],
  },
];

/**
 * Flags that the generated data does not have: `pk-metal sim` parses its two
 * subcommands by hand, so docgen sees no flag set.
 * Source: metal/cmd/pk-metal/main.go (simCmd) and fleet.go (simFleetCmd).
 */
export const handFlags: Record<string, Flag[]> = {
  'sim serve': [
    { name: 'fixture', type: 'string', default: 'generic', usage: 'name of a built-in fixture (generic, huawei-2288h-v5, supermicro-x12) or the path of a fixture YAML file', global: false },
    { name: 'listen', type: 'string', default: '127.0.0.1:8000', usage: 'listen address', global: false },
    { name: 'tls', type: 'bool', default: '', usage: 'serve HTTPS with a temporary self-signed certificate', global: false },
    { name: 'verbose', type: 'bool', default: '', usage: 'log each request (default: only the requests that change state)', global: false },
  ],
  'sim fleet': [
    { name: 'config', type: 'string', default: '', usage: 'fleet file (YAML): the machines, their BMC addresses, identities and backends (required)', global: false },
    { name: 'state-listen', type: 'string', default: '127.0.0.1:8440', usage: 'address of the read-only state endpoint (plain HTTP); an empty value serves none', global: false },
    { name: 'hang-timeout', type: 'duration', default: '', usage: 'power cycle a virtual machine one time when its console shows only firmware output for this long after a power-on (0: never)', global: false },
    { name: 'verbose', type: 'bool', default: '', usage: 'log each request (default: only the requests that change state)', global: false },
  ],
};

/** `--yes`, or `-n` for a flag with a one-letter name. Go accepts one or two dashes for each. */
export function flagText(f: Flag): string {
  return f.name.length === 1 ? `-${f.name}` : `--${f.name}`;
}

/** The anchor of a flag row: `--yes`, or `now.--wait` for the flag of a subcommand. */
export function flagId(f: Flag, sub?: string): string {
  return sub ? `${sub}.${flagText(f)}` : flagText(f);
}

/** The value placeholder after a flag: nothing for a bool, `<duration>` and so on for the rest. */
export function flagValue(f: Flag): string {
  if (f.type === 'bool') return '';
  // Go takes the placeholder from a back-quoted word in the usage text; a
  // phrase there (`talosctl gen secrets`) is not a type.
  return /\s/.test(f.type) ? 'file' : f.type;
}
