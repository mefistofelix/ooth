package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
)

type scmHost struct {
	run func(context.Context, func()) error
	err error
}

func runSCM(name, path string, logger *slog.Logger) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("-service requires an absolute -config path")
	}
	host := &scmHost{run: func(ctx context.Context, ready func()) error {
		return run(ctx, path, logger, ready)
	}}
	if err := svc.Run(name, host); err != nil {
		return err
	}
	return host.err
}

func (host *scmHost) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	status := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 10000}
	statuses <- status
	go func() { done <- host.run(ctx, func() { close(ready) }) }()
	heartbeat := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ready:
			ready = nil
			if ctx.Err() == nil {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				statuses <- status
			}
		case request := <-requests:
			switch request.Cmd {
			case svc.Stop, svc.Shutdown:
				if ctx.Err() == nil {
					status = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 10000}
					statuses <- status
					cancel()
				}
			case svc.Interrogate:
				statuses <- status
			}
		case <-heartbeat.C:
			if status.State == svc.StartPending || status.State == svc.StopPending {
				status.CheckPoint++
				statuses <- status
			}
		case host.err = <-done:
			if host.err != nil {
				return true, 1
			}
			return false, 0
		}
	}
}
