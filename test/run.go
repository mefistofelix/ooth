//go:build ignore

// run keeps private-package tests under test/ using Go's source overlay.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func run() error {
	if len(os.Args) < 2 || (os.Args[1] != "test" && os.Args[1] != "vet") {
		return errors.New("usage: go run ./test/run.go test|vet [go flags and packages]")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	tests, err := filepath.Glob(filepath.Join(root, "test", "*_test.go"))
	if err != nil {
		return err
	}
	if len(tests) == 0 {
		return errors.New("run from the repository root: no test/*_test.go files found")
	}
	replacements := make(map[string]string, len(tests))
	for _, path := range tests {
		replacements[filepath.Join(root, "src", filepath.Base(path))] = path
	}
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0755); err != nil {
		return err
	}
	overlay, err := os.CreateTemp(filepath.Join(root, "tmp"), "go-overlay-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(overlay.Name())
	err = json.NewEncoder(overlay).Encode(struct{ Replace map[string]string }{replacements})
	closeErr := overlay.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	compiler := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		compiler += ".exe"
	}
	args := append([]string{"-C", filepath.Join(root, "src"), os.Args[1], "-overlay", overlay.Name()}, os.Args[2:]...)
	cmd := exec.Command(compiler, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func main() {
	if err := run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
