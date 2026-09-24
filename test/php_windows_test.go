package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Preserve the stock-runtime incompatibility probe, separately from the
// positive proxy suite using the invalid-output-handle launcher.
func TestStockPHPWindowsInheritedDetection(t *testing.T) {
	php := os.Getenv("OOTH_TEST_PHP_CGI")
	if php == "" {
		t.Skip("set OOTH_TEST_PHP_CGI to verify stock Windows FastCGI detection")
	}
	listener, err := OpenListener("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	file, err := listener.ChildFile()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, php, "-n", "-d", "cgi.force_redirect=0")
	cmd.Stdin = file
	cmd.NewProcessGroup = true
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	owner, err := newProcessOwner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	job, err := owner.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	if err := owner.Wait(job); err != nil || !strings.Contains(strings.ToLower(output.String()), "content-type:") {
		t.Fatalf("stock PHP inherited-stdin detection changed; review the launcher: %v %q", err, output.String())
	}
	t.Log("stock PHP chose CGI with ordinary stdout/stderr; FastCGI needs invalid output handles")
}
