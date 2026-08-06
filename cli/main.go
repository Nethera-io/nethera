package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	if hasHelpFlag(os.Args[1:]) {
		entry := findLongestCommandHelp(os.Args[1:])
		if entry == nil {
			printUsage()
			return
		}
		printCommandHelp(*entry)
		return
	}
	if os.Args[1] == "--version" || os.Args[1] == "version" {
		runVersion()
		return
	}

	switch os.Args[1] {
	case "help", "--help", "-h":
		runHelp(os.Args[2:])
	case "login":
		runLogin(os.Args[2:])
	case "whoami":
		runWhoami(os.Args[2:])
	case "logout":
		runLogout(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
	case "target":
		runTarget(os.Args[2:])
	case "apply":
		runDeploy(os.Args[2:])
	case "deploy":
		runDeploy(os.Args[2:])
	case "destroy":
		runDestroy(os.Args[2:])
	case "endpoint":
		runEndpoint(os.Args[2:])
	case "machine":
		runMachine(os.Args[2:])
	case "usage":
		runUsage(os.Args[2:])
	case "logs":
		runLogs(os.Args[2:])
	case "copy":
		runCopy(os.Args[2:])
	case "sync":
		runSync(os.Args[2:])
	case "ls":
		runRemoteLS(os.Args[2:])
	case "secrets":
		runSecrets(os.Args[2:])
	default:
		printUsage()
		os.Exit(1)
	}
}

var cliBuildVersion = "0.1.0"

func runVersion() {
	fmt.Printf("neth %s\n", cliBuildVersion)
	if currentEnvironmentName() != "prod" {
		fmt.Printf("Environment: %s\n", currentEnvironmentName())
		fmt.Printf("API: %s\n", defaultBackendURL())
	}
}
