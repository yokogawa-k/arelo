package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/pflag"
	"golang.org/x/xerrors"
)

var (
	usage = `Usage: %s [OPTION]... -- COMMAND
Run the COMMAND and restart when a file matches the pattern has been modified.

Options:
`
	targets  = pflag.StringSliceP("target", "t", nil, "observation target `path`. (default \"./\")")
	patterns = pflag.StringSliceP("pattern", "p", nil, "trigger pathname `glob` pattern. (required)")
	ignores  = pflag.StringSliceP("ignore", "i", nil, "ignore pathname `glob` pattern.")
	delay    = pflag.DurationP("delay", "d", time.Second, "`duration` to delay the restart of the command.")
	verbose  = pflag.BoolP("verbose", "v", false, "verbose output.")
	help     = pflag.BoolP("help", "h", false, "show this document.")
)

func main() {
	pflag.Parse()
	cmd := pflag.Args()
	if *targets == nil {
		*targets = []string{"./"}
	}
	logVerbose("command:  %q", cmd)
	logVerbose("targets:  %q", targets)
	logVerbose("patterns: %q", patterns)
	logVerbose("ignores:  %q", ignores)
	logVerbose("delay:    %v", delay)

	if len(cmd) == 0 {
		fmt.Fprintf(os.Stderr, "%s: COMMAND required.\n", os.Args[0])
		*help = true
	} else if len(*patterns) == 0 {
		fmt.Fprintf(os.Stderr, "%s: pattern required.\n", os.Args[0])
		*help = true
	}
	if *help {
		fmt.Fprintf(os.Stderr, usage, os.Args[0])
		pflag.PrintDefaults()
		return
	}

	modC, errC, err := watcher(*targets, *ignores, *patterns, *delay)
	if err != nil {
		log.Fatalf("wacher error: %v", err)
	}
	restartC := runner(cmd, *delay)

	for {
		select {
		case name, ok := <-modC:
			if !ok {
				log.Fatalf("wacher closed")
				return
			}
			log.Printf("modified: %q", name)
			restartC <- struct{}{}
		case err := <-errC:
			log.Fatalf("wacher error: %v", err)
			return
		}
	}
}

func logVerbose(fmt string, args ...interface{}) {
	if *verbose {
		log.Printf(fmt, args...)
	}
}

func watcher(targets, ignores, patterns []string, skip time.Duration) (<-chan string, <-chan error, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}

	for _, t := range targets {
		err := addTarget(w, t)
		if err != nil {
			return nil, nil, err
		}
	}

	modC := make(chan string)
	errC := make(chan error)

	go func() {
		defer close(modC)
		next := time.Now().Add(skip)
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					errC <- xerrors.Errorf("watcher.Events closed")
					return
				}

				name := event.Name

				if ignore, err := matchPatterns(name, ignores); err != nil {
					errC <- xerrors.Errorf("ignore match error: %w", err)
					return
				} else if ignore {
					continue
				}

				if time.Now().After(next) {
					if match, err := matchPatterns(name, patterns); err != nil {
						errC <- xerrors.Errorf("pattern match error: %w", err)
						return
					} else if match {
						modC <- name
						next = time.Now().Add(skip)
					}
				}

				// add watcher if new directory.
				if event.Op == fsnotify.Create {
					fi, err := os.Stat(name)
					if err != nil {
						errC <- err
						return
					}
					if fi.IsDir() {
						err := addDirRecursive(w, fi, name, modC)
						if err != nil {
							errC <- err
							return
						}
					}
				}

			case err, ok := <-w.Errors:
				if !ok {
					errC <- xerrors.Errorf("watcher.Errors closed")
					return
				}
				errC <- xerrors.Errorf("wacher error: %w", err)
				return
			}
		}
	}()

	return modC, errC, nil
}

func matchPatterns(t string, pats []string) (bool, error) {
	for _, p := range pats {
		m, err := doublestar.Match(p, t)
		if err != nil {
			return false, err
		}
		if m {
			return true, nil
		}
	}
	return false, nil
}

func addTarget(w *fsnotify.Watcher, t string) error {
	t = path.Clean(t)
	fi, err := os.Stat(t)
	if err != nil {
		return xerrors.Errorf("stat: %w", err)
	}
	if fi.IsDir() {
		return addDirRecursive(w, fi, t, nil)
	}
	logVerbose("watching target: %q", t)
	return w.Add(t)
}

func addDirRecursive(w *fsnotify.Watcher, fi os.FileInfo, t string, ch chan<- string) error {
	logVerbose("watching target: %q", t)
	err := w.Add(t)
	if err != nil {
		return xerrors.Errorf("wacher add: %w", err)
	}
	fis, err := ioutil.ReadDir(t)
	if err != nil {
		return xerrors.Errorf("read dir: %w", err)
	}
	for _, fi := range fis {
		name := path.Join(t, fi.Name())
		if ignore, err := matchPatterns(name, *ignores); err != nil {
			return xerrors.Errorf("ignore match error: %w", err)
		} else if ignore {
			continue
		}
		if ch != nil {
			if match, err := matchPatterns(name, *patterns); err != nil {
				return xerrors.Errorf("pattern match error: %w", err)
			} else if match {
				ch <- name
			}
		}
		if fi.IsDir() {
			err := addDirRecursive(w, fi, name, ch)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func runner(cmd []string, delay time.Duration) chan<- struct{} {
	restart := make(chan struct{})
	trigger := make(chan struct{}, 1)

	go func() {
		for range restart {
			select {
			case trigger <- struct{}{}:
			default:
			}
		}
	}()

	var pcmd string // command string for display.
	for _, s := range cmd {
		if strings.ContainsAny(s, " \t\"'") {
			s = fmt.Sprintf("%q", s)
		}
		pcmd += " " + s
	}
	pcmd = pcmd[1:]

	go func() {
		for {
			stop := make(chan struct{})
			done := make(chan struct{})

			go func() {
				log.Printf("start: %s", pcmd)
				err := runCmd(cmd, stop)
				if err != nil {
					log.Printf("command error: %v", err)
				} else {
					log.Printf("command exit status 0")
				}
				close(done)
			}()

			// ignore trigger before the command started.
			select {
			case <-trigger:
			default:
			}

			<-trigger
			logVerbose("wait %v", delay)
			time.Sleep(delay)
			close(stop)
			<-done // wait process closed
		}
	}()

	return restart
}

func runCmd(cmd []string, stop <-chan struct{}) error {
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	done := make(chan error)
	go func() {
		done <- c.Wait()
	}()

	select {
	case <-stop:
		err := c.Process.Kill()
		if err != nil {
			return xerrors.Errorf("kill error: %w", err)
		}
		return xerrors.New("process killed")
	case err := <-done:
		if err != nil {
			err = xerrors.Errorf("process exit: %w", err)
		}
		return err
	}
}