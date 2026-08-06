package main

import (
	"fmt"
	"os"
	"strings"
)

type commandHelpEntry struct {
	Command     string
	Summary     string
	Usage       string
	Description string
	Examples    []string
}

var commandHelpEntries = []commandHelpEntry{
	{
		Command:     "help",
		Summary:     "Show CLI help.",
		Usage:       "neth help [command]",
		Description: "Prints the command reference. Use nested commands such as neth help endpoint token create for detailed help.",
		Examples:    []string{"neth help", "neth help deploy", "neth help endpoint token create"},
	},
	{
		Command:     "login",
		Summary:     "Sign in to Nethera.",
		Usage:       "neth login",
		Description: "Opens a browser login flow and stores a CLI session for the configured environment.",
	},
	{
		Command:     "whoami",
		Summary:     "Show the signed-in user and workspace.",
		Usage:       "neth whoami",
		Description: "Prints the current environment, API URL, user email, workspace, role, and plan.",
	},
	{
		Command:     "version",
		Summary:     "Show the CLI version and configured environment.",
		Usage:       "neth version",
		Description: "Prints the installed CLI version, current environment, and API URL.",
	},
	{
		Command:     "logout",
		Summary:     "Clear the CLI session.",
		Usage:       "neth logout",
		Description: "Removes the saved session token for the current environment.",
	},
	{
		Command:     "init",
		Summary:     "Create or complete a nethera.yml project.",
		Usage:       "neth init [dir]",
		Description: "Creates nethera.yml, imports or prompts for Compose content, registers the app when needed, and asks which machines should be targets.",
		Examples:    []string{"neth init", "neth init ./my-app"},
	},
	{
		Command:     "target",
		Summary:     "Change the target machines for a project.",
		Usage:       "neth target [dir]",
		Description: "Reopens the machine target picker and updates targets in the project's nethera.yml.",
		Examples:    []string{"neth target", "neth target ./my-app"},
	},
	{
		Command:     "deploy",
		Summary:     "Deploy the current app.",
		Usage:       "neth deploy [path/to/nethera.yml] [--no-token] [--wait|--replace] [--yes] [--verbose]",
		Description: "Submits the desired nethera.yml state to Nethera and streams deployment logs from the target machine.",
		Examples:    []string{"neth deploy", "neth deploy --verbose", "neth deploy ./nethera.yml --no-token"},
	},
	{
		Command:     "apply",
		Summary:     "Alias for deploy.",
		Usage:       "neth apply [path/to/nethera.yml] [--no-token] [--wait|--replace] [--yes] [--verbose]",
		Description: "Runs the same deployment flow as neth deploy.",
	},
	{
		Command:     "destroy",
		Summary:     "Destroy the deployed app from its target machines.",
		Usage:       "neth destroy [path/to/nethera.yml] [--volumes] [--yes] [--verbose]",
		Description: "Submits a destroy job for the app targets in nethera.yml. Local containers are stopped by Docker Compose on the target machine. Add --volumes to remove Compose-managed named and anonymous volumes.",
	},
	{
		Command:     "endpoint",
		Summary:     "Manage endpoints.",
		Usage:       "neth endpoint token <list|create|revoke>",
		Description: "Manages endpoint API tokens for services configured with auth: token.",
	},
	{
		Command:     "endpoint token",
		Summary:     "Manage endpoint API tokens.",
		Usage:       "neth endpoint token <list|create|revoke>",
		Description: "Lists, creates, and revokes API tokens for services configured with auth: token.",
	},
	{
		Command:     "endpoint token list",
		Summary:     "List endpoint API tokens.",
		Usage:       "neth endpoint token list [--app <APP>] [--service <SERVICE>]",
		Description: "Lists API tokens for auth: token endpoints. The app is inferred from nethera.yml when possible.",
	},
	{
		Command:     "endpoint token create",
		Summary:     "Create an endpoint API token.",
		Usage:       "neth endpoint token create <SERVICE> [--app <APP>] [--name <NAME>] [--expires-at <ISO_TIME>]",
		Description: "Creates a named token for a service endpoint and prints the secret token once.",
		Examples:    []string{"neth endpoint token create api --name \"Production client\""},
	},
	{
		Command:     "endpoint token revoke",
		Summary:     "Revoke an endpoint API token.",
		Usage:       "neth endpoint token revoke <TOKEN_ID> [--yes]",
		Description: "Revokes an endpoint API token after confirmation unless --yes is passed.",
	},
	{
		Command:     "logs",
		Summary:     "Stream app logs.",
		Usage:       "neth logs [--app <APP>] [--service <SERVICE>] [--tail <LINES>] [--machine <MACHINE>] [--deployment <DEPLOYMENT>] [--no-follow]",
		Description: "Streams recent and live Docker Compose logs through the Nethera agent. The app is inferred from nethera.yml when possible.",
		Examples:    []string{"neth logs", "neth logs --service web --tail 200", "neth logs --machine home-gpu --no-follow"},
	},
	{
		Command:     "secrets",
		Summary:     "Manage app secrets.",
		Usage:       "neth secrets <set|list|reveal|delete>",
		Description: "Manages app-scoped secrets. The app is inferred from nethera.yml when possible.",
	},
	{
		Command:     "secrets set",
		Summary:     "Create or update an app secret.",
		Usage:       "neth secrets set <NAME> [VALUE] [--app <APP>] [--value <VALUE>]",
		Description: "Stores an app-scoped secret. If VALUE is omitted, the CLI prompts without echoing input.",
		Examples:    []string{"neth secrets set API_KEY", "neth secrets set API_KEY <secret-value>", "neth secrets set API_KEY --value <secret-value>"},
	},
	{
		Command:     "secrets list",
		Summary:     "List app secret names.",
		Usage:       "neth secrets list [--app <APP>]",
		Description: "Lists secret names and update times. Secret values are not shown.",
	},
	{
		Command:     "secrets reveal",
		Summary:     "Reveal an app secret value.",
		Usage:       "neth secrets reveal <NAME> [--app <APP>]",
		Description: "Prints a secret value for recovery or debugging.",
	},
	{
		Command:     "secrets delete",
		Summary:     "Delete an app secret.",
		Usage:       "neth secrets delete <NAME> [--app <APP>] [--yes]",
		Description: "Deletes an app-scoped secret after confirmation unless --yes is passed.",
	},
	{
		Command:     "machine",
		Summary:     "Manage machines.",
		Usage:       "neth machine <list|stats|pair|select|enable|disable|deregister>",
		Description: "Lists, pairs, selects deployment targets, enables, disables, and deregisters machines in the current workspace.",
	},
	{
		Command:     "machine list",
		Summary:     "List registered machines.",
		Usage:       "neth machine list",
		Description: "Shows machine names, regions, availability, and running app summaries.",
	},
	{
		Command:     "machine stats",
		Summary:     "Show machine health and resource stats.",
		Usage:       "neth machine stats",
		Description: "Prints management state, agent health, tunnel status, Docker status, deployments, CPU, memory, disk, and GPU readiness when available.",
	},
	{
		Command:     "machine pair",
		Summary:     "Pair a machine with Nethera.",
		Usage:       "neth machine pair <pair-code> [machine-name] [--region <REGION>]",
		Description: "Completes the pairing flow started by the agent installer.",
	},
	{
		Command:     "machine select",
		Summary:     "Change the target machines for a project.",
		Usage:       "neth machine select [dir]",
		Description: "Reopens the machine target picker and updates targets in the project's nethera.yml, including for an already initialized project.",
		Examples:    []string{"neth machine select", "neth machine select ./my-app"},
	},
	{
		Command:     "machine enable",
		Summary:     "Enable management for a machine.",
		Usage:       "neth machine enable <machine>",
		Description: "Allows Nethera to deploy, route traffic, stream logs, and manage the machine if plan limits allow it.",
	},
	{
		Command:     "machine disable",
		Summary:     "Disable management for a machine.",
		Usage:       "neth machine disable <machine>",
		Description: "Stops Nethera from managing the machine without deregistering it or stopping local containers.",
	},
	{
		Command:     "machine deregister",
		Summary:     "Deregister a machine.",
		Usage:       "neth machine deregister <machine> [--cleanup-deployments] [--cleanup-wireguard] [--yes]",
		Description: "Removes the machine from Nethera management and revokes its token. Cleanup flags request local cleanup through the agent.",
	},
	{
		Command:     "usage",
		Summary:     "Show monthly usage.",
		Usage:       "neth usage [--month YYYY-MM]",
		Description: "Shows workspace bandwidth and request usage for the current or selected month, with byte counts formatted in KB, MB, GB, or TB.",
		Examples:    []string{"neth usage", "neth usage --month 2026-07"},
	},
	{
		Command:     "copy",
		Summary:     "Copy files to or from a machine.",
		Usage:       "neth copy <local-path> <machine>:/mnt/nethera/... | neth copy <machine>:/mnt/nethera/... <local-path>",
		Description: "Copies files or directories between your local machine and a paired Nethera machine using the agent-coordinated copy channel.",
	},
	{
		Command:     "sync",
		Summary:     "Sync a directory to or from a machine.",
		Usage:       "neth sync <local-dir> <machine>:/mnt/nethera/... | neth sync <machine>:/mnt/nethera/... <local-dir>",
		Description: "Synchronizes new or changed files between a local directory and a paired Nethera machine. It compares size and modification time, and does not delete destination files.",
		Examples:    []string{"neth sync ./music homebox:/mnt/nethera/navidrome/music", "neth sync --dry-run homebox:/mnt/nethera/navidrome/music ./music"},
	},
	{
		Command:     "ls",
		Summary:     "List files on a machine.",
		Usage:       "neth ls <machine>:/mnt/nethera/...",
		Description: "Shows a bounded directory listing for a path on a paired Nethera machine.",
	},
}

func printUsage() {
	fmt.Println("usage:")
	for _, entry := range commandHelpEntries {
		fmt.Printf("  %s\n", entry.Usage)
	}
	fmt.Println()
	fmt.Println("Run `neth help <command>` for details.")
}

func runHelp(args []string) {
	if len(args) == 1 && args[0] == "--docs-mdx" {
		fmt.Print(renderCLIReferenceMDX())
		return
	}
	query := strings.Join(args, " ")
	if strings.TrimSpace(query) == "" {
		printUsage()
		return
	}
	entry := findCommandHelp(query)
	if entry == nil {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", query)
		printUsage()
		os.Exit(1)
	}
	printCommandHelp(*entry)
}

func findCommandHelp(query string) *commandHelpEntry {
	query = strings.TrimSpace(query)
	for _, entry := range commandHelpEntries {
		if entry.Command == query {
			return &entry
		}
	}
	for _, entry := range commandHelpEntries {
		if strings.HasPrefix(entry.Command, query+" ") {
			return &entry
		}
	}
	return nil
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func findLongestCommandHelp(args []string) *commandHelpEntry {
	parts := []string{}
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			break
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		parts = append(parts, arg)
	}
	for len(parts) > 0 {
		if entry := findCommandHelp(strings.Join(parts, " ")); entry != nil {
			return entry
		}
		parts = parts[:len(parts)-1]
	}
	return nil
}

func printCommandHelp(entry commandHelpEntry) {
	fmt.Println(entry.Usage)
	fmt.Println()
	fmt.Println(entry.Summary)
	if strings.TrimSpace(entry.Description) != "" {
		fmt.Println(entry.Description)
	}
	if len(entry.Examples) > 0 {
		fmt.Println()
		fmt.Println("examples:")
		for _, example := range entry.Examples {
			fmt.Printf("  %s\n", example)
		}
	}
}

func renderCLIReferenceMDX() string {
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.WriteString("title: CLI\n")
	builder.WriteString("description: Command reference for the neth CLI.\n")
	builder.WriteString("---\n\n")
	builder.WriteString("Install the CLI on your laptop or development machine:\n\n")
	builder.WriteString("<InstallCliCommand />\n\n")
	builder.WriteString("Use `neth help <command>` for the same reference in your terminal.\n\n")

	sections := []struct {
		Title    string
		Commands []string
	}{
		{Title: "Help", Commands: []string{"help"}},
		{Title: "Auth", Commands: []string{"login", "whoami", "version", "logout"}},
		{Title: "Projects", Commands: []string{"init", "target"}},
		{Title: "Deployments", Commands: []string{"deploy", "apply", "destroy"}},
		{Title: "Endpoint API Tokens", Commands: []string{"endpoint", "endpoint token", "endpoint token list", "endpoint token create", "endpoint token revoke"}},
		{Title: "Logs", Commands: []string{"logs"}},
		{Title: "Secrets", Commands: []string{"secrets", "secrets set", "secrets list", "secrets reveal", "secrets delete"}},
		{Title: "Machines", Commands: []string{"machine", "machine list", "machine stats", "machine pair", "machine select", "machine enable", "machine disable", "machine deregister"}},
		{Title: "Usage", Commands: []string{"usage"}},
		{Title: "File Transfer", Commands: []string{"copy", "sync", "ls"}},
	}
	for _, section := range sections {
		builder.WriteString("## " + section.Title + "\n\n")
		for _, command := range section.Commands {
			entry := findCommandHelp(command)
			if entry == nil {
				continue
			}
			builder.WriteString("### `neth " + entry.Command + "`\n\n")
			builder.WriteString("```bash\n")
			if strings.Contains(entry.Usage, " | ") {
				for _, usage := range strings.Split(entry.Usage, " | ") {
					builder.WriteString(usage + "\n")
				}
			} else {
				builder.WriteString(entry.Usage + "\n")
			}
			builder.WriteString("```\n\n")
			if strings.TrimSpace(entry.Description) != "" {
				builder.WriteString(entry.Description + "\n\n")
			} else {
				builder.WriteString(entry.Summary + "\n\n")
			}
			if len(entry.Examples) > 0 {
				builder.WriteString("Examples:\n\n")
				builder.WriteString("```bash\n")
				for _, example := range entry.Examples {
					builder.WriteString(example + "\n")
				}
				builder.WriteString("```\n\n")
			}
		}
	}
	return builder.String()
}
