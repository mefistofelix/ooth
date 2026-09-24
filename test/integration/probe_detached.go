//go:build ignore

package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

func main() {
	cmd := exec.Command("tmp/ooth-no-console.test.exe", "-test.run=TestGracefulAndForceful", "-test.timeout=15s")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, NoInheritHandles: true}
	if err := cmd.Run(); err != nil {
		panic(err)
	}
	fmt.Println("PASS: no console and no inherited standard handles")
}
