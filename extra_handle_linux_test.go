package main

import (
	"os"
	"os/exec"
	"testing"
)

func extraTestHandle(t *testing.T, cmd *exec.Cmd, file *os.File) (string, func()) {
	t.Helper()
	cmd.ExtraFiles = []*os.File{file}
	return "3", func() {}
}
