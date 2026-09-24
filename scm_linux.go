package main

import (
	"fmt"
	"log/slog"
	"time"
)

func runSCM(_, _ string, _ *slog.Logger) error {
	return fmt.Errorf("SCM service mode requires Windows")
}

func (*manager) spawnSCM(*service, time.Time) error {
	return fmt.Errorf("SCM service control requires Windows")
}
