package cli

import "testing"

func TestRegisterAndLookup(t *testing.T) {
	Reset()
	Register(Command{Name: "demo", Run: func([]string) int { return 7 }})
	c, ok := Lookup("demo")
	if !ok || c.Run(nil) != 7 {
		t.Fatal("demo not registered/runnable")
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("unexpected hit")
	}
}
