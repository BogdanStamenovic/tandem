package main

import (
	"os"
	osexec "os/exec"
)

func exec(name string, args ...string) error {
	c := osexec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}
