package setup

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Asker asks questions on a terminal, or returns defaults without one.
// Every answer it takes is printed, so a non-interactive log shows exactly
// what was chosen.
type Asker struct {
	Interactive bool
	in          *bufio.Reader
	out         io.Writer
}

func NewAsker(forceNonInteractive bool) *Asker {
	interactive := !forceNonInteractive && isTTY(os.Stdin.Fd())
	return &Asker{Interactive: interactive, in: bufio.NewReader(os.Stdin), out: os.Stdout}
}

func isTTY(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}

func (a *Asker) Say(format string, args ...any) { fmt.Fprintf(a.out, format+"\n", args...) }

// String asks for a value. validate may reject it (interactive: ask again).
func (a *Asker) String(q, def string, validate func(string) error) (string, error) {
	if !a.Interactive {
		if validate != nil && def != "" {
			if err := validate(def); err != nil {
				return "", fmt.Errorf("%s: default %q invalid: %v", q, def, err)
			}
		}
		a.Say("  %s: %s (default, no terminal)", q, orNone(def))
		return def, nil
	}
	for {
		if def != "" {
			fmt.Fprintf(a.out, "%s [%s]: ", q, def)
		} else {
			fmt.Fprintf(a.out, "%s: ", q)
		}
		line, err := a.in.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("%s: no answer: %w", q, err)
		}
		v := strings.TrimSpace(line)
		if v == "" {
			v = def
		}
		if validate != nil {
			if err := validate(v); err != nil {
				a.Say("  %v", err)
				continue
			}
		}
		return v, nil
	}
}

// Choice asks for one of options (first letter or full word).
func (a *Asker) Choice(q string, options []string, def string) (string, error) {
	return a.String(q+" ("+strings.Join(options, "/")+")", def, func(v string) error {
		for _, o := range options {
			if strings.EqualFold(v, o) {
				return nil
			}
		}
		return fmt.Errorf("answer one of: %s", strings.Join(options, ", "))
	})
}

// YesNo asks a yes/no question.
func (a *Asker) YesNo(q string, def bool) (bool, error) {
	d := "n"
	if def {
		d = "y"
	}
	v, err := a.String(q+" (y/n)", d, func(v string) error {
		switch strings.ToLower(v) {
		case "y", "yes", "n", "no":
			return nil
		}
		return fmt.Errorf("answer y or n")
	})
	if err != nil {
		return false, err
	}
	v = strings.ToLower(v)
	return v == "y" || v == "yes", nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
