package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigExtraFields(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	appPath := filepath.Join(directory, "app.yaml")
	main := "watch: [app.yaml]\nresources: {max_cpu_percent: 85}\n"
	app := "name: web\ncommand: [worker]\nlisten: {network: tcp, address: '127.0.0.1:12345'}\nrestart_on: [{glob: '*.py'}]\n"
	write(t, path, main)
	write(t, appPath, app)
	want, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "watch: [app.yaml]\nresources: {max_cpu_percent: 85, vendor_limit: [1, 2]}\nmetadata: {project: demo}\n")
	write(t, appPath, "name: web\ncommand: [worker]\nlisten: {network: tcp, address: '127.0.0.1:12345', vendor: {enabled: true}}\nrestart_on: [{glob: '*.py', description: sources}]\nframework: {routes: [home, admin]}\n")
	got, err := Load(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("extra fields changed configuration: %v\n%+v", err, got)
	}
	for _, invalid := range []struct{ main, app string }{
		{"watch: [app.yaml]\nresources: {max_cpu_percent: 101}\n", app},
		{main, app + "min_workers: invalid\n"},
		{main, app + "max_workers: 0\n"},
		{main, app + "metadata: [unterminated\n"},
		{main, app + "---\nmetadata: another-document\n"},
		{main, app + "name: duplicate\n"},
	} {
		write(t, path, invalid.main)
		write(t, appPath, invalid.app)
		if _, err := Load(path); err == nil {
			t.Fatal("permissive fields must not disable YAML or known-field validation")
		}
	}
}

func TestRecursiveGlob(t *testing.T) {
	for _, test := range []struct {
		pattern, path string
		want          bool
	}{
		{"apps/**/ooth.yaml", "apps/ooth.yaml", true},
		{"apps/**/ooth.yaml", "apps/a/b/ooth.yaml", true},
		{"apps/**/ooth.yaml", "apps/a/b/other.yaml", false},
		{"apps/*/ooth.yaml", "apps/a/b/ooth.yaml", false},
		{"apps/*/ooth.yaml", "apps/ooth.yaml", false},
		{"apps/**/src/**/?.[jp]s", "apps/a/src/b/c/x.js", true},
		{"apps/**/src/**/?.[jp]s", "apps/src/x.ps", true},
		{"apps/**/src/**/?.[jp]s", "apps/src/xx.js", false},
		{"apps/**/**/ooth.yaml", "apps/ooth.yaml", true},
	} {
		if got := matchGlob(filepath.FromSlash(test.pattern), filepath.FromSlash(test.path)); got != test.want {
			t.Errorf("match %q against %q = %v, want %v", test.pattern, test.path, got, test.want)
		}
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "ooth.yaml")
	write(t, path, "watch: ['apps/**/ooth.yaml', 'apps/*/ooth.yaml']\n")
	for index, relative := range []string{"apps/ooth.yaml", "apps/one/ooth.yaml", "apps/a/b/ooth.yaml"} {
		write(t, filepath.Join(directory, filepath.FromSlash(relative)), fmt.Sprintf("name: app%d\ncommand: [worker]\n", index))
	}
	write(t, filepath.Join(directory, "apps", "a", "ignore.txt"), "not yaml: [")
	snapshot, err := Load(path)
	if err != nil || len(snapshot.Apps) != 3 {
		t.Fatalf("recursive discovery/overlap: %d apps, %v", len(snapshot.Apps), err)
	}
	write(t, path, "watch: ['missing/**/ooth.yaml']\n")
	if snapshot, err := Load(path); err != nil || len(snapshot.Apps) != 0 {
		t.Fatalf("future directory glob: %d apps, %v", len(snapshot.Apps), err)
	}
	write(t, path, "watch: ['apps/**/[broken']\n")
	if _, err := Load(path); err == nil {
		t.Fatal("accepted malformed recursive glob")
	}
}

func TestRestartOnRecursiveFiles(t *testing.T) {
	directory := t.TempDir()
	app := restartApp(filepath.Join(directory, "**", "*.txt"))
	app.Env["TEST_OOTH_VIRTUAL"] = "1"
	log, _ := runRestartFixture(t, app)
	nested := filepath.Join(directory, "new", "deep", "settings.txt")
	for index, source := range []string{nested, nested, filepath.Join(directory, "root.txt")} {
		write(t, source, fmt.Sprint(index))
		waitRestart(t, "recursive source did not restart app", func() bool {
			return log.state("worker").telemetry == index+2
		})
	}
	if err := os.Remove(nested); err != nil {
		t.Fatal(err)
	}
	waitRestart(t, "recursive remove did not restart app", func() bool {
		return log.state("worker").telemetry == 5
	})
}
