package main

import (
	"fmt"
	"log/slog"
)

func runSCM(_, _ string, _ *slog.Logger) error {
	return fmt.Errorf("SCM service mode requires Windows")
}
