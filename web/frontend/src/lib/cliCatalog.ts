import catalog from "@/data/cli-catalog.json";

// Roles with a CLI; entries may carry `roles`, and absent means every server role. A companion's own short CLI
// (companion_radio MyMesh::handleCommand) is listed only where an entry names it.
export type CliRole = "repeater" | "sensor" | "room" | "companion";

export interface CliCommand {
  command: string;
  hint?: string;
  roles?: CliRole[];
}

export interface CliConfigKey {
  key: string;
  hint?: string;
  roles?: CliRole[];
}

// Mirror of CommonCLI.cpp + each role's MyMesh.cpp; a trailing space pre-fills the argument position.
export const CLI_TOPLEVEL_COMMANDS = catalog.topLevelCommands as CliCommand[];
export const CLI_CONFIG_KEYS = catalog.configKeys as CliConfigKey[];

const forRole = <T extends { roles?: CliRole[] }>(items: T[], role: CliRole) =>
  items.filter((i) => (i.roles ? i.roles.includes(role) : role !== "companion"));

export function cliCommandsFor(role: CliRole): CliCommand[] {
  return forRole(CLI_TOPLEVEL_COMMANDS, role);
}

export function cliConfigKeysFor(role: CliRole): CliConfigKey[] {
  return forRole(CLI_CONFIG_KEYS, role);
}
