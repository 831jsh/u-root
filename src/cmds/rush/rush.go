// Copyright 2012 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
sh reads in a line at a time and runs it.
prompt is '% '
*/

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"
)

type builtin func(c *Command) error

// TODO: probably have one builtin map and use it for both types?
var (
	urpath   = "/go/bin:/ubin:/buildbin:/bin:/usr/local/bin:"
	builtins = make(map[string]builtin)
	// Some builtins really want to be forked off, esp. in the busybox case.
	forkBuiltins = make(map[string]builtin)
	// the environment dir is INTENDED to be per-user and bound in
	// a private name space at /env.
	envDir = "/env"
)

func addBuiltIn(name string, f builtin) error {
	if _, ok := builtins[name]; ok {
		return errors.New(fmt.Sprintf("%v already a builtin", name))
	}
	builtins[name] = f
	return nil
}

func addForkBuiltIn(name string, f builtin) error {
	if _, ok := builtins[name]; ok {
		return errors.New(fmt.Sprintf("%v already a forkBuiltin", name))
	}
	forkBuiltins[name] = f
	return nil
}

func wire(cmds []*Command) error {
	for i, c := range cmds {
		// IO defaults.
		var err error
		if c.Stdin == nil {
			if c.Stdin, err = OpenRead(c, os.Stdin, 0); err != nil {
				return err
			}
		}
		if c.link != "|" {
			if c.Stdout, err = OpenWrite(c, os.Stdout, 1); err != nil {
				return err
			}
		}
		if c.Stderr, err = OpenWrite(c, os.Stderr, 2); err != nil {
			return err
		}
		// The validation is such that "|" is not set on the last one.
		// Also, there won't be redirects and "|" inappropriately.
		if c.link != "|" {
			continue
		}
		w, err := cmds[i+1].StdinPipe()
		if err != nil {
			return err
		}
		r, err := cmds[i].StdoutPipe()
		if err != nil {
			return err
		}
		// Oh, yuck.
		// There seems to be no way to do the classic
		// inherited pipes thing in Go. Hard to believe.
		go func() {
			io.Copy(w, r)
			w.Close()
		}()
	}
	return nil
}

func runit(c *Command) error {
	if b, ok := builtins[c.cmd]; ok {
		if err := b(c); err != nil {
			return err
		}
	} else {
		if err := c.Start(); err != nil {
			return errors.New(fmt.Sprintf("%v: Path %v\n", err, os.Getenv("PATH")))
		}
		if err := c.Wait(); err != nil {
			return errors.New(fmt.Sprintf("wait: %v:\n", err))
		}
	}
	return nil
}

func OpenRead(c *Command, r io.Reader, fd int) (io.Reader, error) {
	if c.fdmap[fd] != "" {
		return os.Open(c.fdmap[fd])
	}
	return r, nil
}
func OpenWrite(c *Command, w io.Writer, fd int) (io.Writer, error) {
	if c.fdmap[fd] != "" {
		return os.Create(c.fdmap[fd])
	}
	return w, nil
}

func doArgs(cmds []*Command) error {
	for _, c := range cmds {
		globargv := []string{}
		for _, v := range c.args {
			if v.mod == "ENV" {
				e := v.val
				if !path.IsAbs(v.val) {
					e = path.Join(envDir, e)
				}
				b, err := ioutil.ReadFile(e)
				if err != nil {
					return err
				}
				// It goes in as one argument. Not sure if this is what we want
				// but it gets very weird to start splitting it on spaces. Or maybe not?
				globargv = append(globargv, string(b))
			} else if globs, err := filepath.Glob(v.val); err == nil && len(globs) > 0 {
				globargv = append(globargv, globs...)
			} else {
				globargv = append(globargv, v.val)
			}
		}

		c.cmd = globargv[0]
		c.argv = globargv[1:]
	}
	return nil
}

// There seems to be no harm in creating a Cmd struct
// even for builtins, so for now, we do.
// It will, however, do a path lookup, which we really don't need,
// and we may change it later.
func commands(cmds []*Command) error {
	for _, c := range cmds {
		c.Cmd = exec.Command(c.cmd, c.argv[:]...)
		// this is a Very Special Case related to a Go issue.
		// we're not able to unshare correctly in builtin.
		// Not sure of the issue but this hack will have to do until
		// we understand it. Barf.
		if c.cmd == "builtin" {
			s := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
			c.Cmd.SysProcAttr = s
		}
	}
	return nil
}
func command(c *Command) error {
	// for now, bg will just happen in background.
	if c.bg {
		go func() {
			if err := runit(c); err != nil {
				fmt.Fprintf(os.Stderr, "%v", err)
			}
		}()
	} else {
		err := runit(c)
		return err
	}
	return nil
}

func main() {
	b := bufio.NewReader(os.Stdin)

	defer func() {
		switch err := recover().(type) {
		case nil:
		case error:
			log.Fatalf("Bummer: %v", err)
		default:
			log.Fatalf("unexpected panic value: %T(%v)", err, err)
		}
		_, _, _ = getCommand(b)
	}()
	if f, ok := forkBuiltins[os.Args[0]]; ok {
		if err := f(&Command{cmd: os.Args[0], Cmd: &exec.Cmd{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, argv: os.Args[1:]}); err != nil {
			log.Fatalf("%v", err)
		}
		os.Exit(0)
	}

	if len(os.Args) != 1 {
		fmt.Println("no scripts/args yet")
		os.Exit(1)
	}

	fmt.Printf("%% ")
	for {
		cmds, status, err := getCommand(b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		if err := doArgs(cmds); err != nil {
			fmt.Fprintf(os.Stderr, "args problem: %v\n", err)
			continue
		}
		if err := commands(cmds); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}
		if err := wire(cmds); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}
		for i := range cmds {
			if err := command(cmds[i]); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				if cmds[i].link == "||" {
					continue
				}
				// yes, not needed, but useful so you know
				// what goes on here.
				if cmds[i].link == "&&" {
					break
				}
				break
			} else {
				if cmds[i].link == "||" {
					break
				}
			}
		}
		if status == "EOF" {
			break
		}
		fmt.Printf("%% ")
	}
}