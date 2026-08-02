package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func runSecrets(args []string) {
	if len(args) == 0 {
		printSecretsUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "set":
		runSecretsSet(args[1:])
	case "list":
		runSecretsList(args[1:])
	case "reveal":
		runSecretsReveal(args[1:])
	case "delete":
		runSecretsDelete(args[1:])
	default:
		printSecretsUsage()
		os.Exit(1)
	}
}

func printSecretsUsage() {
	fmt.Println("usage:")
	fmt.Println("  neth secrets set <NAME> [VALUE] [--app <APP>] [--value <VALUE>]")
	fmt.Println("  neth secrets list [--app <APP>]")
	fmt.Println("  neth secrets reveal <NAME> [--app <APP>]")
	fmt.Println("  neth secrets delete <NAME> [--app <APP>] [--yes]")
}

func runSecretsSet(args []string) {
	fs := flag.NewFlagSet("secrets set", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	appOverride := fs.String("app", "", "app id or name")
	valueFlag := fs.String("value", "", "secret value; may be recorded in shell history")
	fs.Parse(args)
	name := strings.TrimSpace(fs.Arg(0))
	if err := validateSecretName(name); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	positionalValue := ""
	if fs.NArg() > 1 {
		positionalValue = fs.Arg(1)
	}
	if fs.NArg() > 2 {
		fmt.Println("too many arguments; use: neth secrets set <NAME> [VALUE]")
		os.Exit(1)
	}
	if positionalValue != "" && *valueFlag != "" {
		fmt.Println("provide the secret value either as a positional argument or with --value, not both")
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	app, err := resolveSecretAppContext(*backendURL, token, *appOverride)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	value := *valueFlag
	if value == "" {
		value = positionalValue
	}
	if value == "" {
		value, err = promptSecretValue("Secret value: ")
		if err != nil {
			fmt.Printf("failed to read secret value: %v\n", err)
			os.Exit(1)
		}
	}
	if strings.ContainsAny(value, "\r\n") {
		fmt.Println("Multiline secret values are not supported yet.")
		os.Exit(1)
	}
	if _, err := putAppSecret(*backendURL, token, app.ID, name, value); err != nil {
		fmt.Printf("failed to save secret: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Secret saved for app: %s.\n", app.Name)
	fmt.Println("Redeploy this app for the change to take effect.")
}

func runSecretsList(args []string) {
	fs := flag.NewFlagSet("secrets list", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	appOverride := fs.String("app", "", "app id or name")
	fs.Parse(args)
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	app, err := resolveSecretAppContext(*backendURL, token, *appOverride)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	secrets, err := listAppSecrets(*backendURL, token, app.ID)
	if err != nil {
		fmt.Printf("failed to list secrets: %v\n", err)
		os.Exit(1)
	}
	appName := app.Name
	if strings.TrimSpace(secrets.AppName) != "" {
		appName = secrets.AppName
	}
	fmt.Printf("Secrets for app: %s\n\n", appName)
	if len(secrets.Secrets) == 0 {
		fmt.Println("No secrets set.")
		return
	}
	fmt.Printf("%-24s %s\n", "NAME", "UPDATED")
	for _, secret := range secrets.Secrets {
		fmt.Printf("%-24s %s\n", secret.Name, formatTimestamp(secret.UpdatedAt))
	}
}

func runSecretsReveal(args []string) {
	fs := flag.NewFlagSet("secrets reveal", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	appOverride := fs.String("app", "", "app id or name")
	fs.Parse(args)
	name := strings.TrimSpace(fs.Arg(0))
	if err := validateSecretName(name); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	app, err := resolveSecretAppContext(*backendURL, token, *appOverride)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	secret, err := revealAppSecret(*backendURL, token, app.ID, name)
	if err != nil {
		fmt.Printf("failed to reveal secret: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(secret.Value)
}

func runSecretsDelete(args []string) {
	fs := flag.NewFlagSet("secrets delete", flag.ExitOnError)
	backendURL := fs.String("backend", defaultBackendURL(), "backend base URL")
	appOverride := fs.String("app", "", "app id or name")
	yes := fs.Bool("yes", false, "delete without confirmation")
	fs.Parse(args)
	name := strings.TrimSpace(fs.Arg(0))
	if err := validateSecretName(name); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	token, err := loadAuthToken(*backendURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	app, err := resolveSecretAppContext(*backendURL, token, *appOverride)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if !*yes {
		confirmed, confirmErr := promptYesNoDefaultNo(fmt.Sprintf("Delete secret %s from app %s? Future deploys requiring this secret may fail.", name, app.Name))
		if confirmErr != nil {
			fmt.Printf("failed to read confirmation: %v\n", confirmErr)
			os.Exit(1)
		}
		if !confirmed {
			fmt.Println("Secret unchanged.")
			return
		}
	}
	if err := deleteAppSecret(*backendURL, token, app.ID, name); err != nil {
		fmt.Printf("failed to delete secret: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Secret deleted.")
}
