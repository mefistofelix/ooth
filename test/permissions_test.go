package main

import (
	"testing"

	"github.com/goccy/go-yaml"
)

func TestPermissionConfig(t *testing.T) {
	for input, expected := range map[string]Permissions{
		"0660": 0660, "0o660": 0660, "'0660'": 0660, "'660'": 0660, "0": 0,
		"'u=rw,g=rw,o='": 0660, "'a+rw'": 0666, "'g-w'": 0640,
		"'u=rw,go=r'": 0644, "'a=,u+rw,g+r'": 0640,
		"'a+rwx,o-rwx'": 0770, "'a='": 0,
	} {
		t.Run(input, func(t *testing.T) {
			var app App
			if err := yaml.Unmarshal([]byte("socket_mode: "+input), &app); err != nil {
				t.Fatal(err)
			}
			if app.SocketMode == nil || *app.SocketMode != expected {
				t.Fatalf("%s: got %v, expected %04o", input, app.SocketMode, expected)
			}
			data, err := yaml.Marshal(app)
			if err != nil {
				t.Fatal(err)
			}
			var decoded App
			if err := yaml.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.SocketMode == nil || *decoded.SocketMode != expected {
				t.Fatal("permission YAML round trip changed its value")
			}
		})
	}
	for _, input := range []string{"-1", "01777", "'0888'", "'0999'", "'bad'", "'u+z'", "'a+s'", "'u=rw,'", "'u+g'", "'rw-rw----'", "[]", "true", "1.5", "''"} {
		var app App
		if err := yaml.Unmarshal([]byte("socket_mode: "+input), &app); err == nil {
			t.Fatalf("accepted invalid mode %s", input)
		}
	}
}
