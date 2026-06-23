package product

import (
	"testing"

	"flow/internal/cli"
)

func TestRegisterCommandsRegistersProductVerbs(t *testing.T) {
	cli.Reset()
	registerCommands()

	for _, name := range []string{"ui", "serve", "attention", "slack"} {
		if _, ok := cli.Lookup(name); !ok {
			t.Fatalf("%s was not registered", name)
		}
	}
	if serve, _ := cli.Lookup("serve"); !serve.Hidden {
		t.Fatal("serve should stay hidden")
	}
}
