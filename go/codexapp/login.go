package codexapp

import (
	"io"
	"os"
	"os/exec"
)

// Streams are the standard streams handed to an interactive Codex command. In a terminal program they are the
// process's own os.Stdin, os.Stdout and os.Stderr, which exec passes to the child as-is, so Codex talks to the
// terminal directly. A Stdin that is not an *os.File is copied to the child until the child exits; Login does not
// wait for a read from it still under way then, and the bytes that read returns are dropped.
type Streams struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Login runs `<bin> login`, or `<bin> login --device-auth`, with cfg's executable, CODEX_HOME, working directory and
// environment hygiene and the caller's streams, and waits for it. The flow itself is never read.
func Login(cfg Config, deviceAuth bool, s Streams) error {
	env, err := prepare(cfg)
	if err != nil {
		return err
	}
	args := []string{"login"}
	if deviceAuth {
		args = append(args, "--device-auth")
	}
	// No CommandContext: an interrupt at the terminal reaches Codex through the process group, and Codex decides how
	// its own flow ends.
	cmd := exec.Command(cfg.Bin, args...)
	cmd.Env = ChildEnv(env, cfg.Bin, cfg.CodexHome)
	cmd.Dir = cfg.Dir
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	// exec's own copy of a Stdin that is not a file would keep Wait until a read from it returns, however long after
	// the child exits: this one is not waited for, and Wait closes the pipe it writes to.
	var stdin io.WriteCloser
	if _, isFile := s.Stdin.(*os.File); isFile || s.Stdin == nil {
		cmd.Stdin = s.Stdin
	} else if stdin, err = cmd.StdinPipe(); err != nil {
		return codexError("`codex login` did not complete", err)
	}
	if err := cmd.Start(); err != nil {
		return codexError("`codex login` did not complete", err)
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(stdin, s.Stdin)
			_ = stdin.Close()
		}()
	}
	if err := cmd.Wait(); err != nil {
		return codexError("`codex login` did not complete", err)
	}
	return nil
}
