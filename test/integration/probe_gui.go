//go:build ignore

package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	marker := "tmp/gui-marker"
	os.Remove(marker)
	cmd := exec.Command("tmp/GuiProbe.exe", marker)
	cmd.NewProcessGroup = true
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(marker)
		if string(data) == "ready" {
			break
		}
		if time.Now().After(deadline) {
			panic("GUI startup timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		panic(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			panic(err)
		}
	case <-time.After(3 * time.Second):
		panic("WM_CLOSE timeout")
	}
	data, _ := os.ReadFile(marker)
	if string(data) != "closed" {
		panic("FormClosing not observed")
	}
	fmt.Println("PASS: GUI received WM_CLOSE and ran FormClosing before exiting")
}
