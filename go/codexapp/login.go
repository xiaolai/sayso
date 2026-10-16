package codexapp

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Streams are the standard streams handed to an interactive Codex command. In a terminal program they are the
// process's own os.Stdin, os.Stdout and os.Stderr, which exec passes to the child as-is, so Codex talks to the
// terminal directly.
type Streams struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Login runs `<bin> login`, or `<bin> login --device-auth`, with cfg's executable, CODEX_HOME and environment hygiene
// and the caller's streams, and waits for it. The flow itself is never read.
func Login(cfg Config, deviceAuth bool, s Streams) error {
	if !filepath.IsAbs(cfg.Bin) {
		return fmt.Errorf("codexapp: executable path %q is not absolute", cfg.Bin)
	}
	if cfg.Guard != nil {
		if err := cfg.Guard(cfg.Bin); err != nil {
			return fmt.Errorf("codexapp: start refused: %w", err)
		}
	}
	env := cfg.Env.List()
	if cfg.Env.IsZero() {
		env = os.Environ()
	}
	args := []string{"login"}
	if deviceAuth {
		args = append(args, "--device-auth")
	}
	// No CommandContext: an interrupt at the terminal reaches Codex through the process group, and Codex decides how
	// its own flow ends.
	cmd := exec.Command(cfg.Bin, args...)
	cmd.Env = ChildEnv(env, cfg.Bin, cfg.CodexHome)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	if err := cmd.Run(); err != nil {
		return codexError("`codex login` did not complete", err)
	}
	return nil
}
