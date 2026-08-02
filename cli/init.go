package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
)

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	fs.Parse(args)

	dirPath := strings.TrimSpace(fs.Arg(0))
	if dirPath == "" {
		dirPath = "."
	}

	absDir, err := filepath.Abs(dirPath)
	if err != nil {
		fmt.Printf("failed to resolve directory: %v\n", err)
		os.Exit(1)
	}

	manifestPath := filepath.Join(absDir, "nethera.yml")
	var existingManifest netheraManifest
	manifestExists := false
	if _, err := os.Stat(manifestPath); err == nil {
		manifestExists = true
		existingManifest, err = loadManifest(manifestPath)
		if err != nil {
			fmt.Printf("failed to read manifest: %v\n", err)
			os.Exit(1)
		}
		if len(existingManifest.Targets) > 0 && strings.TrimSpace(existingManifest.Compose) != "" {
			fmt.Printf("already set up: %s\n", manifestPath)
			os.Exit(0)
		}
	} else if !os.IsNotExist(err) {
		fmt.Printf("failed to inspect manifest: %v\n", err)
		os.Exit(1)
	}

	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	machines, err := listMachines(*backendURL, token)
	if err != nil {
		fmt.Printf("failed to list machines: %v\n", err)
		os.Exit(1)
	}
	if len(machines) == 0 {
		fmt.Println("no machines registered to your account")
		os.Exit(1)
	}

	composeContent := strings.TrimSpace(existingManifest.Compose)
	sourceDescription := "updated existing nethera.yml"
	if composeContent == "" {
		var err error
		composeContent, sourceDescription, err = promptInitialCompose(absDir)
		if err != nil {
			fmt.Printf("failed to prepare compose content: %v\n", err)
			os.Exit(1)
		}
	}

	selectedTargets := existingManifest.Targets
	if len(selectedTargets) == 0 {
		selectedTargets = promptDeploymentTargets(machines)
	}

	appName := strings.TrimSpace(existingManifest.AppName)
	if appName == "" {
		appName = defaultAppNameFromDir(absDir)
	}
	manifest := netheraManifest{
		AppName: appName,
		AppID:   existingManifest.AppID,
		Targets: selectedTargets,
		Compose: composeContent,
	}
	if err := saveManifest(manifestPath, manifest); err != nil {
		fmt.Printf("failed to save manifest: %v\n", err)
		os.Exit(1)
	}

	if manifestExists {
		fmt.Printf("updated manifest at %s (%s)\n", manifestPath, sourceDescription)
	} else {
		fmt.Printf("saved manifest to %s (%s)\n", manifestPath, sourceDescription)
	}
	fmt.Println("targets:")
	for _, target := range selectedTargets {
		fmt.Printf(" - %s\n", target)
	}
}

func runTarget(args []string) {
	fs := flag.NewFlagSet("target", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	fs.Parse(args)

	dirPath := strings.TrimSpace(fs.Arg(0))
	if dirPath == "" {
		dirPath = "."
	}

	absDir, err := filepath.Abs(dirPath)
	if err != nil {
		fmt.Printf("failed to resolve directory: %v\n", err)
		os.Exit(1)
	}
	manifestPath, err := resolveNetheraManifestPath(absDir)
	if err != nil {
		fmt.Printf("failed to resolve nethera manifest: %v\n", err)
		os.Exit(1)
	}
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		fmt.Printf("failed to read manifest: %v\n", err)
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	machines, err := listMachines(*backendURL, token)
	if err != nil {
		fmt.Printf("failed to list machines: %v\n", err)
		os.Exit(1)
	}
	if len(machines) == 0 {
		fmt.Println("no machines registered to your account")
		os.Exit(1)
	}
	selectedTargets := promptDeploymentTargets(machines)
	manifest.Targets = selectedTargets
	if err := saveManifest(manifestPath, manifest); err != nil {
		fmt.Printf("failed to save manifest: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("updated targets in %s\n", manifestPath)
	fmt.Println("targets:")
	for _, target := range selectedTargets {
		fmt.Printf(" - %s\n", target)
	}
}

func promptDeploymentTargets(machines []machineSummary) []string {
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		return promptDeploymentTargetsInteractive(machines)
	}
	return promptDeploymentTargetsFallback(machines)
}

func promptDeploymentTargetsFallback(machines []machineSummary) []string {
	fmt.Println("Select one or more machines as deployment targets:")
	for i, machine := range machines {
		fmt.Printf("  %d. %s - %s - apps: %s\n", i+1, machine.Name, machineAvailabilityLabel(machine), machineAppsLabel(machine))
	}
	printUnavailableMachineNote(machines)

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter numbers separated by commas (for example: 1,3): ")
	choiceLine, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("failed to read selection: %v\n", err)
		os.Exit(1)
	}
	choiceLine = strings.TrimSpace(choiceLine)
	if choiceLine == "" {
		fmt.Println("at least one selection is required")
		os.Exit(1)
	}

	selectedTargets := []string{}
	parts := strings.Split(choiceLine, ",")
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		index, err := strconv.Atoi(trimmed)
		if err != nil || index < 1 || index > len(machines) {
			fmt.Printf("invalid selection: %s\n", trimmed)
			os.Exit(1)
		}
		selectedTargets = append(selectedTargets, machines[index-1].Name)
	}
	if len(selectedTargets) == 0 {
		fmt.Println("at least one selection is required")
		os.Exit(1)
	}
	return selectedTargets
}

func promptDeploymentTargetsInteractive(machines []machineSummary) []string {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return promptDeploymentTargetsFallback(machines)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	selected := make([]bool, len(machines))
	cursor := 0
	renderedLines := 0
	statusLine := ""

	redraw := func() {
		if renderedLines > 0 {
			fmt.Printf("\x1b[%dA", renderedLines)
		}
		lines := targetPickerLines(machines, selected, cursor, statusLine)
		for _, line := range lines {
			fmt.Print("\x1b[2K\r")
			fmt.Println(line)
		}
		renderedLines = len(lines)
	}

	fmt.Print("\x1b[?25l")
	defer fmt.Print("\x1b[?25h")
	redraw()

	buffer := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buffer)
		if err != nil {
			fmt.Println()
			fmt.Printf("failed to read selection: %v\n", err)
			os.Exit(1)
		}
		key := buffer[:n]
		statusLine = ""
		switch {
		case len(key) == 1 && key[0] == 3:
			fmt.Println()
			os.Exit(130)
		case len(key) == 1 && (key[0] == '\r' || key[0] == '\n'):
			targets := selectedMachineNames(machines, selected)
			if len(targets) == 0 {
				statusLine = "\x1b[33mSelect at least one machine with Space.\x1b[0m"
				redraw()
				continue
			}
			fmt.Println()
			return targets
		case len(key) == 1 && key[0] == ' ':
			selected[cursor] = !selected[cursor]
		case len(key) == 1 && (key[0] == 'k' || key[0] == 'K'):
			if cursor > 0 {
				cursor--
			}
		case len(key) == 1 && (key[0] == 'j' || key[0] == 'J'):
			if cursor < len(machines)-1 {
				cursor++
			}
		case len(key) == 3 && key[0] == 27 && key[1] == '[' && key[2] == 'A':
			if cursor > 0 {
				cursor--
			}
		case len(key) == 3 && key[0] == 27 && key[1] == '[' && key[2] == 'B':
			if cursor < len(machines)-1 {
				cursor++
			}
		}
		redraw()
	}
}

func targetPickerLines(machines []machineSummary, selected []bool, cursor int, statusLine string) []string {
	lines := []string{
		"Select one or more machines as deployment targets:",
		"  Use ↑/↓ to move, Space to select, Enter to continue.",
	}
	if hasUnavailableMachines(machines) {
		lines = append(lines, "  \x1b[33m●\x1b[0m Offline machines can be targeted; deployment will roll out when they come online.")
	}
	lines = append(lines, "")
	for i, machine := range machines {
		pointer := " "
		if i == cursor {
			pointer = "›"
		}
		checkbox := "[ ]"
		if selected[i] {
			checkbox = "[✓]"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s \x1b[2mapps: %s\x1b[0m", pointer, checkbox, machineStatusDot(machine), machineTargetLabel(machine), machineAppsLabel(machine)))
	}
	lines = append(lines, "", statusLine)
	return lines
}

func selectedMachineNames(machines []machineSummary, selected []bool) []string {
	targets := []string{}
	for i, machine := range machines {
		if selected[i] {
			targets = append(targets, machine.Name)
		}
	}
	return targets
}

func machineTargetLabel(machine machineSummary) string {
	region := strings.TrimSpace(machine.RegionCode)
	if region == "" {
		region = "unknown"
	}
	return fmt.Sprintf("%s \x1b[2m[%s]\x1b[0m \x1b[2m%s\x1b[0m", machine.Name, region, machineAvailabilityLabel(machine))
}

func machineStatusDot(machine machineSummary) string {
	if machine.IsAvailable {
		return "\x1b[32m●\x1b[0m"
	}
	return "\x1b[33m●\x1b[0m"
}

func machineAvailabilityLabel(machine machineSummary) string {
	if machine.IsAvailable {
		return "available"
	}
	return "offline"
}

func machineAppsLabel(machine machineSummary) string {
	if len(machine.RunningApps) == 0 {
		return "no running apps"
	}
	apps := machine.RunningApps
	if len(apps) > 4 {
		return strings.Join(apps[:4], ", ") + ", ..."
	}
	return strings.Join(apps, ", ")
}

func hasUnavailableMachines(machines []machineSummary) bool {
	for _, machine := range machines {
		if !machine.IsAvailable {
			return true
		}
	}
	return false
}

func printUnavailableMachineNote(machines []machineSummary) {
	if hasUnavailableMachines(machines) {
		fmt.Println("Note: offline machines can be targeted; deployment will roll out when they come online.")
	}
}
