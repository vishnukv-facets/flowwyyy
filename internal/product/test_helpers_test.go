package product

import (
	"io"
	"os"
	"testing"
)

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldOut := os.Stdout
	oldErr := os.Stderr
	os.Stdout = wOut
	os.Stderr = wErr
	done := make(chan string, 2)
	go func() {
		b, _ := io.ReadAll(rOut)
		done <- string(b)
	}()
	go func() {
		b, _ := io.ReadAll(rErr)
		done <- string(b)
	}()
	f()
	wOut.Close()
	wErr.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr
	a := <-done
	b := <-done
	return a + b
}
