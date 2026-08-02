package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("nethera-agent %s\n", agentVersion())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		runEnroll(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "uninstall" {
		runUninstall(os.Args[2:])
		return
	}
	runDaemon(os.Args[1:])
}
