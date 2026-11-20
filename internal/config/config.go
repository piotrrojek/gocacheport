package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

func Parse(flags *flag.FlagSet, args []string) error {
	var envErr error
	flags.VisitAll(func(setting *flag.Flag) {
		if envErr != nil {
			return
		}
		name := "GOCACHEPORT_" + strings.ToUpper(strings.ReplaceAll(setting.Name, "-", "_"))
		if value, ok := os.LookupEnv(name); ok {
			if err := flags.Set(setting.Name, value); err != nil {
				envErr = fmt.Errorf("invalid value for environment variable %s", name)
			}
		}
	})
	if envErr != nil {
		return envErr
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	return nil
}

func Token(path string) (string, error) {
	token := os.Getenv("GOCACHEPORT_TOKEN")
	if path == "" {
		if strings.ContainsAny(token, "\r\n") {
			return "", errors.New("bearer token must not contain line breaks")
		}
		return token, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token = strings.TrimSpace(string(body))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("token file must contain one nonempty token")
	}
	return token, nil
}
