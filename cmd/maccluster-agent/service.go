package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const agentServiceLabel = "com.maccluster.agent"

type launchctlRunner func(...string) ([]byte, error)

func runLaunchctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/launchctl", args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("launchctl %s timed out; inspect service status before retrying: %w", args[0], ctx.Err())
	}
	return out, err
}

func serviceCommand(args []string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("service commands require macOS launchd")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if commandRole == "tools" {
		binary = filepath.Join(filepath.Dir(binary), "maccluster-agent")
	}
	return manageService(args, home, binary, fmt.Sprintf("gui/%d", os.Getuid()), os.Stdout, runLaunchctl)
}

func manageService(args []string, home, binary, domain string, output io.Writer, run launchctlRunner) error {
	if len(args) == 0 {
		return errors.New("usage: maccluster-agent service install|start|stop|restart|status|config")
	}
	action := args[0]
	if action != "install" && action != "start" && action != "stop" && action != "restart" && action != "status" && action != "config" {
		return fmt.Errorf("unknown service action %q", action)
	}
	flags := flag.NewFlagSet("service "+action, flag.ContinueOnError)
	dir := filepath.Join(home, "Library", "Application Support", "MacCluster")
	// Older UI versions append this option to every command. Lifecycle actions
	// always manage the installed job; only install/config use the supplied path.
	flags.StringVar(&dir, "state-dir", dir, "private state directory (used by install/config)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected service arguments")
	}
	job := domain + "/" + agentServiceLabel
	plist := filepath.Join(home, "Library", "LaunchAgents", agentServiceLabel+".plist")
	call := func(args ...string) error {
		out, err := run(args...)
		if err != nil {
			return fmt.Errorf("launchctl %s: %w: %s", args[0], err, bytes.TrimSpace(out))
		}
		if action == "status" {
			_, err = output.Write(out)
		}
		return err
	}
	stop := func() error {
		err := call("bootout", job)
		if err == nil {
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, lookupErr := run("print", job)
				var missing *exec.ExitError
				if errors.As(lookupErr, &missing) && missing.ExitCode() == 113 {
					return nil
				}
				if lookupErr != nil {
					return fmt.Errorf("cannot confirm service is stopped: %w", lookupErr)
				}
				if time.Now().After(deadline) {
					return errors.New("service is still unloading; inspect service status before retrying")
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		var exited *exec.ExitError
		if !errors.As(err, &exited) || (exited.ExitCode() != 3 && exited.ExitCode() != 113) {
			return err
		}
		_, lookupErr := run("print", job)
		var missing *exec.ExitError
		if errors.As(lookupErr, &missing) && missing.ExitCode() == 113 {
			return nil
		}
		if lookupErr != nil {
			return fmt.Errorf("cannot confirm service is stopped: %w", lookupErr)
		}
		return err
	}
	bootstrap := func() error {
		for attempt := 0; ; attempt++ {
			err := call("bootstrap", domain, plist)
			if err == nil {
				return nil
			}
			// bootout can return while launchd is still retiring the old job.
			// The retiring job may still be visible to print. Do not kickstart it:
			// wait for bootstrap to load the requested plist instead.
			var exit *exec.ExitError
			if attempt >= 10 || !errors.As(err, &exit) || exit.ExitCode() != 5 {
				return err
			}
			_, lookupErr := run("print", job)
			var missing *exec.ExitError
			if lookupErr != nil && (!errors.As(lookupErr, &missing) || missing.ExitCode() != 113) {
				return err
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	switch action {
	case "config":
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		_, err = io.WriteString(output, servicePlist(binary, absDir))
		return err
	case "status":
		return call("print", job)
	case "stop":
		if err := stop(); err != nil {
			return err
		}
		fmt.Fprintln(output, "Agent unloaded; workloads remain running. It will start again at next login.")
		return nil
	case "install":
		if _, err := run("print", job); err == nil {
			return errors.New("service is loaded; run service stop before installing a different configuration")
		} else {
			// Only launchctl's 'service not found' exit permits installation. A timeout
			// or permission error must not be mistaken for an unloaded service.
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 113 {
				return fmt.Errorf("cannot inspect existing service: %w", err)
			}
		}
		var err error
		dir, err = filepath.Abs(dir)
		if err != nil {
			return err
		}
		binary, err = filepath.EvalSymlinks(binary)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err = os.Chmod(dir, 0700); err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(plist), ".maccluster-*.plist")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		_, err = io.WriteString(file, servicePlist(binary, dir))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(file.Name(), plist); err != nil {
			return err
		}
		fmt.Fprintln(output, "LaunchAgent installed. Run service start to start it.")
		return nil
	case "restart":
		if _, err := os.Stat(plist); err != nil {
			return fmt.Errorf("service configuration unavailable; run service install first: %w", err)
		}
		if err := stop(); err != nil {
			return err
		}
		return bootstrap()
	default: // start an existing job, or bootstrap the installed plist.
		if _, err := os.Stat(plist); err != nil {
			return fmt.Errorf("service configuration unavailable; run service install first: %w", err)
		}
		if _, err := run("print", job); err == nil {
			return call("kickstart", job)
		} else {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 113 {
				return fmt.Errorf("cannot inspect existing service: %w", err)
			}
		}
		return bootstrap()
	}
}

// serve exits only 0 or 1 (main.go); status 137 is reserved here for SIGKILL.
// The shell stays outside the agent process, so it can observe an uncatchable kill.
// Handle TERM even during startup and retry wait when a trap interrupts it.
const agentSupervisor = `child=; stopping=0; trap 'stopping=1; if [ -n "$child" ]; then kill -TERM "$child" 2>/dev/null || :; fi' TERM INT; "$@" & child=$!; if [ "$stopping" -eq 1 ]; then kill -TERM "$child" 2>/dev/null || :; fi; while :; do wait "$child"; result=$?; if ! kill -0 "$child" 2>/dev/null; then break; fi; done; if [ "$result" -eq 137 ]; then exit 0; fi; exit 1`

func servicePlist(binary, dir string) string {
	escape := func(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + agentServiceLabel + `</string>
<key>ProgramArguments</key><array><string>/bin/sh</string><string>-c</string><string>` + escape(agentSupervisor) + `</string><string>maccluster-supervisor</string><string>` + escape(binary) + `</string><string>serve</string><string>--state-dir</string><string>` + escape(dir) + `</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>AbandonProcessGroup</key><true/><key>ProcessType</key><string>Background</string>
<key>Umask</key><integer>63</integer>
</dict></plist>
`
}
