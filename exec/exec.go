package exec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/argoproj/pkg/rand"
)

var (
	ErrWaitPIDTimeout = fmt.Errorf("Timed out waiting for PID to complete")
	Unredacted        = Redact(nil)
)

type CmdError struct {
	Args   string
	Stderr string
	Cause  error
}

func (ce *CmdError) Error() string {
	res := fmt.Sprintf("`%v` failed %v", ce.Args, ce.Cause)
	if ce.Stderr != "" {
		res = fmt.Sprintf("%s: %s", res, ce.Stderr)
	}
	return res
}

func (ce *CmdError) String() string {
	return ce.Error()
}

func newCmdError(args string, cause error, stderr string) *CmdError {
	return &CmdError{Args: args, Stderr: stderr, Cause: cause}
}

// TimeoutBehavior defines behavior for when the command takes longer than the passed in timeout to exit
// By default, SIGKILL is sent to the process and it is not waited upon
type TimeoutBehavior struct {
	// Signal determines the signal to send to the process
	Signal syscall.Signal
	// ShouldWait determines whether to wait for the command to exit once timeout is reached
	ShouldWait bool
}

type CmdOpts struct {
	// Timeout determines how long to wait for the command to exit
	Timeout time.Duration
	// Redactor redacts tokens from the output
	Redactor func(text string) string
	// TimeoutBehavior configures what to do in case of timeout
	TimeoutBehavior TimeoutBehavior
	// SkipErrorLogging defines whether to skip logging of execution errors (rc > 0)
	SkipErrorLogging bool
	// CaptureStderr defines whether to capture stderr in addition to stdout
	CaptureStderr bool
}

var DefaultCmdOpts = CmdOpts{
	Timeout:          time.Duration(0),
	Redactor:         Unredacted,
	TimeoutBehavior:  TimeoutBehavior{syscall.SIGKILL, false},
	SkipErrorLogging: false,
	CaptureStderr:    false,
}

func Redact(items []string) func(text string) string {
	return func(text string) string {
		for _, item := range items {
			text = strings.Replace(text, item, "******", -1)
		}
		return text
	}
}

// RunCommandExt is a convenience function to run/log a command and return/log stderr in an error upon
// failure.
func RunCommandExt(cmd *exec.Cmd, opts CmdOpts) (string, error) {
	execId, err := rand.RandString(5)
	if err != nil {
		return "", err
	}
	logCtx := log.WithFields(log.Fields{"execID": execId})

	redactor := DefaultCmdOpts.Redactor
	if opts.Redactor != nil {
		redactor = opts.Redactor
	}

	// log in a way we can copy-and-paste into a terminal
	args := strings.Join(cmd.Args, " ")
	logCtx.WithFields(log.Fields{"dir": cmd.Dir}).Info(redactor(args))

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err = cmd.Start()
	if err != nil {
		return "", err
	}

	done := make(chan error)
	go func() { done <- cmd.Wait() }()

	// Start a timer
	timeout := DefaultCmdOpts.Timeout

	if opts.Timeout != time.Duration(0) {
		timeout = opts.Timeout
	}

	var timoutCh <-chan time.Time
	if timeout != 0 {
		timoutCh = time.NewTimer(timeout).C
	}

	timeoutBehavior := DefaultCmdOpts.TimeoutBehavior
	if opts.TimeoutBehavior.Signal != syscall.Signal(0) {
		timeoutBehavior = opts.TimeoutBehavior
	}

	select {
	// noinspection ALL
	case <-timoutCh:
		_ = cmd.Process.Signal(timeoutBehavior.Signal)
		if timeoutBehavior.ShouldWait {
			<-done
		}
		output := stdout.String()
		if opts.CaptureStderr {
			output += stderr.String()
		}
		logCtx.WithFields(log.Fields{"duration": time.Since(start)}).Debug(redactor(output))
		err = newCmdError(redactor(args), fmt.Errorf("timeout after %v", timeout), "")
		logCtx.Error(err.Error())
		return strings.TrimSuffix(output, "\n"), err
	case err := <-done:
		if err != nil {
			output := stdout.String()
			if opts.CaptureStderr {
				output += stderr.String()
			}
			logCtx.WithFields(log.Fields{"duration": time.Since(start)}).Debug(redactor(output))
			err := newCmdError(redactor(args), errors.New(redactor(err.Error())), strings.TrimSpace(redactor(stderr.String())))
			if !opts.SkipErrorLogging {
				logCtx.Error(err.Error())
			}
			return strings.TrimSuffix(output, "\n"), err
		}
	}

	output := stdout.String()
	if opts.CaptureStderr {
		output += stderr.String()
	}
	logCtx.WithFields(log.Fields{"duration": time.Since(start)}).Debug(redactor(output))

	return strings.TrimSuffix(output, "\n"), nil
}

func RunCommand(name string, opts CmdOpts, arg ...string) (string, error) {
	return RunCommandExt(exec.Command(name, arg...), opts)
}

// WaitPIDOpts are options to WaitPID
type WaitPIDOpts struct {
	PollInterval time.Duration
	Timeout      time.Duration
}

// WaitPID waits for a non-child process id to exit
func WaitPID(pid int, opts ...WaitPIDOpts) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("platform '%s' unsupported", runtime.GOOS)
	}
	var timeout time.Duration
	pollInterval := time.Second
	if len(opts) > 0 {
		if opts[0].PollInterval != 0 {
			pollInterval = opts[0].PollInterval
		}
		if opts[0].Timeout != 0 {
			timeout = opts[0].Timeout
		}
	}
	path := fmt.Sprintf("/proc/%d", pid)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var timoutCh <-chan time.Time
	if timeout != 0 {
		timoutCh = time.NewTimer(timeout).C
	}
	for {
		select {
		case <-ticker.C:
			_, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
		case <-timoutCh:
			return ErrWaitPIDTimeout
		}
	}
}
