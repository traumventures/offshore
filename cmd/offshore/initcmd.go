package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/initcfg"
)

// initCmd prints (or with --write writes) a repository configuration
// detected from the repository's Cargo workspace, CI workflows and
// Dockerfiles.
func initCmd(args []string, stdout io.Writer) error {
	fs := newFlagSet("init")
	write := fs.Bool("write", false, "write "+config.RepoFileName+" instead of printing it")
	force := fs.Bool("force", false, "with --write: replace an existing "+config.RepoFileName)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("usage: offshore init [--write [--force]] [DIR]")
	}
	root := fs.Arg(0)
	if root == "" {
		root = repoTop()
	}
	d, err := initcfg.Detect(root)
	if err != nil {
		return err
	}
	out := d.Render()
	if !*write {
		_, err := io.WriteString(stdout, out)
		return err
	}
	path := filepath.Join(d.Root, config.RepoFileName)
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s exists (use --force to replace it)", path)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return err
	}
	if _, err := config.LoadRepo(d.Root); err != nil {
		return fmt.Errorf("wrote %s, but it does not validate: %w", path, err)
	}
	fmt.Fprintf(stdout, "wrote %s; review it, then run `offshore doctor`\n", path)
	return nil
}

// repoTop is the git top level of the working directory, else the working
// directory itself.
func repoTop() string {
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	wd, _ := os.Getwd()
	return wd
}
