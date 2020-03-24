package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const exitStopped uint32 = 0x1337

var (
	// ErrEmptyCommand is an error returned when attempting to start a Process that has an empty 'Args' array.
	ErrEmptyCommand = errors.New("process arguments are empty")
	// ErrNotCompleted is returned when attempting to access the exit code on a running process or wait on a non-stared proess.
	ErrNotCompleted = errors.New("the process has not yet completed or was not started")
	// ErrAlreadyStarted is an error returned by the 'Start' or 'Run' functions when attempting to start a process that has
	// already been started via a 'Start' or 'Run' function call.
	ErrAlreadyStarted = errors.New("process has already been started")
	// ErrNotSupportedOS is an error that is returned when a non-Windows device attempts a Windows only function.
	ErrNotSupportedOS = errors.New("function is avaliable on Windows only")
	// ErrNoProcessFound is returned by the SetParent* functions on Windows devices when a specified parent process
	// could not be found.
	ErrNoProcessFound = errors.New("could not find a suitable parent process")

	errStdinSet  = errors.New("process Stdin already set")
	errStderrSet = errors.New("process Stderr already set")
	errStdoutSet = errors.New("process Stdout already set")
)

// Process is a struct that represents an executable command and allows for setting
// options in order change the operating functions.
type Process struct {
	Dir     string
	Env     []string
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Timeout time.Duration

	ctx     context.Context
	err     error
	opts    *options
	exit    uint32
	once    uint32
	flags   uint32
	reader  *os.File
	cancel  context.CancelFunc
	parent  *container
	closers []*os.File
}

// ExitError is a type of error that is returned by the Wait and Run functions when a function
// returns an error code other than zero.
type ExitError struct {
	Exit uint32
}

// Run will start the process and wait until it completes. This function will return the same errors as the 'Start'
// function if they occur or the 'Wait' function if any errors occur during Process runtime.
func (p *Process) Run() error {
	if err := p.Start(); err != nil {
		return err
	}
	return p.Wait()
}

// Wait will block until the Process completes or is terminated by a call to Stop. This function will return
// 'ErrNotCompleted' if the Process has not been started.
func (p *Process) Wait() error {
	if !p.isStarted() {
		return ErrNotCompleted
	}
	if p.ctx.Err() == nil {
		<-p.ctx.Done()
	}
	return p.err
}

// Error returns any errors that may have occurred during Process operation.
func (p Process) Error() error {
	return p.err
}

// Start will attempt to start the Process and will return an errors that occur while starting the Process.
// This function will return 'ErrEmptyCommand' if the 'Args' parameter is empty or nil and 'ErrAlreadyStarted'
// if attempting to start a Process that already has been started previously.
func (p *Process) Start() error {
	if p.Running() || p.isStarted() {
		return ErrAlreadyStarted
	}
	if len(p.Args) == 0 {
		return ErrEmptyCommand
	}
	if p.ctx == nil {
		p.ctx = context.Background()
	}
	if p.cancel == nil {
		if p.Timeout > 0 {
			p.ctx, p.cancel = context.WithTimeout(p.ctx, p.Timeout)
		} else {
			p.ctx, p.cancel = context.WithCancel(p.ctx)
		}
	}
	if p.reader != nil {
		p.reader.Close()
		p.reader = nil
	}
	if err := startProcess(p); err != nil {
		return p.stopWith(err)
	}
	return nil
}

// Running returns true if the current Process is running, false otherwise.
func (p Process) Running() bool {
	return p.isStarted() && p.ctx != nil && p.ctx.Err() == nil
}

// String returns the command and arguments that this Process will execute.
func (p Process) String() string {
	return strings.Join(p.Args, " ")
}

// Error fulfills the error interface and retruns a formatted string that specifies the Process Exit Code.
func (e ExitError) Error() string {
	return fmt.Sprintf("process exit: %d", e.Exit)
}
func (p *Process) stopWith(e error) error {
	if atomic.LoadUint32(&p.once) == 0 {
		atomic.StoreUint32(&p.once, 1)
		if p.opts != nil {
			p.opts.close()
		}
		if p.closers != nil && len(p.closers) > 0 {
			for i := range p.closers {
				p.closers[i].Close()
				p.closers[i] = nil
			}
			p.closers = nil
		}
		if p.ctx.Err() != nil && p.exit == 0 {
			p.err = p.ctx.Err()
			p.exit = exitStopped
		}
	}
	p.cancel()
	if p.err == nil && p.ctx.Err() != nil {
		if e != nil {
			p.err = e
			return e
		}
		return nil
	}
	return p.err
}

// ExitCode returns the Exit Code of the process. If the Process is still running or has not been started, this
// function returns an 'ErrNotCompleted' error.
func (p Process) ExitCode() (int32, error) {
	if p.isStarted() && p.Running() {
		return 0, ErrNotCompleted
	}
	return int32(p.exit), nil
}

// Output runs the Process and returns its standard output. Any returned error will usually be of type *ExitError.
func (p *Process) Output() ([]byte, error) {
	if p.Stdout != nil {
		return nil, errStdoutSet
	}
	var b bytes.Buffer
	p.Stdout = &b
	err := p.Run()
	return b.Bytes(), err
}

// CombinedOutput runs the Process and returns its combined standard output and standard error.
func (p *Process) CombinedOutput() ([]byte, error) {
	if p.Stdout != nil {
		return nil, errStdoutSet
	}
	if p.Stderr != nil {
		return nil, errStderrSet
	}
	var b bytes.Buffer
	p.Stdout = &b
	p.Stderr = &b
	err := p.Run()
	return b.Bytes(), err
}

// StdinPipe returns a pipe that will be connected to the Processes's standard input when the Process starts.
// The pipe will be closed automatically after the Processes starts. A caller need only call Close to force
// the pipe to close sooner.
func (p *Process) StdinPipe() (io.WriteCloser, error) {
	if p.isStarted() {
		return nil, ErrAlreadyStarted
	}
	if p.Stdin != nil {
		return nil, errStdinSet
	}
	var err error
	if p.Stdin, p.reader, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("unable to create Pipe: %w", err)
	}
	return p.reader, nil
}

// StdoutPipe returns a pipe that will be connected to the Processes's
// standard output when the Processes starts.
//
// The pipe will be closed after the Processe exits, so most callers need not close the pipe themselves.
// It is thus incorrect to call Wait before all reads from the pipe have completed. For the same reason, it is
// incorrect to use Run when using StderrPipe.
//
// See the StdoutPipe example for idiomatic usage.
func (p *Process) StdoutPipe() (io.ReadCloser, error) {
	if p.isStarted() {
		return nil, ErrAlreadyStarted
	}
	if p.Stdout != nil {
		return nil, errStdoutSet
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("unable to create Pipe: %w", err)
	}
	p.Stdout = w
	p.closers = append(p.closers, w)
	return r, nil
}

// StderrPipe returns a pipe that will be connected to the Processes's
// standard error when the Processes starts.
//
// The pipe will be closed after the Processe exits, so most callers need not close the pipe themselves.
// It is thus incorrect to call Wait before all reads from the pipe have completed. For the same reason, it is
// incorrect to use Run when using StderrPipe.
//
// See the StdoutPipe example for idiomatic usage.
func (p *Process) StderrPipe() (io.ReadCloser, error) {
	if p.isStarted() {
		return nil, ErrAlreadyStarted
	}
	if p.Stdout != nil {
		return nil, errStderrSet
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("unable to create Pipe: %w", err)
	}
	p.Stderr = w
	p.closers = append(p.closers, w)
	return r, nil
}