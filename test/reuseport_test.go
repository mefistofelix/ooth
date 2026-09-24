package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// These are ordinary process pools with no ooth listen setting. Only the worker
// knows it is using reuseport; the manager has no mode or platform branch for it.
func TestWorkerOwnedListeners(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native TCP reuseport pool requires Linux for these runtimes")
	}
	for _, name := range []string{"PYTHON", "NODE", "BUN", "DENO", "SOCKETIFY", "TRUEASYNC"} {
		protocols := []string{"http1"}
		if name == "NODE" || name == "DENO" || name == "TRUEASYNC" {
			protocols = append(protocols, "h2c")
		}
		for _, protocol := range protocols {
			for _, proxy := range []string{"direct", "caddy", "nginx"} {
				t.Run(name+"/"+protocol+"/"+proxy, func(t *testing.T) {
					executable := os.Getenv("OOTH_TEST_" + name)
					if executable == "" {
						t.Skip("set OOTH_TEST_" + name)
					}
					executable, _ = filepath.Abs(executable)
					command := []string{executable}
					workerEnv := map[string]string{"TEST_BIND_URL": "{{.vars.endpoint}}"}
					script := "../test/integration/node_worker.cjs"
					switch name {
					case "PYTHON":
						script = "../test/integration/reuseport/python.py"
					case "BUN", "DENO":
						script = "../test/integration/reuseport/native.mjs"
						if name == "DENO" {
							command = append(command, "run", "-A", "--unstable-net")
						}
					case "SOCKETIFY":
						script = "../test/integration/socketify/worker.py"
						workerEnv["TEST_OOTH_PROTOCOL"] = "1"
					case "TRUEASYNC":
						script = "../test/integration/trueasync/worker.php"
						command = append(command, "-n", "-d", "display_errors=stderr")
					}
					script, _ = filepath.Abs(script)
					command = append(command, script, "{{.vars.endpoint}}")
					if protocol == "h2c" {
						workerEnv["TEST_NODE_H2C"] = "1"
					}
					runRuntimeLifecycle(t, name, protocol, proxy, command, workerEnv, false)
				})
			}
		}
	}
}
