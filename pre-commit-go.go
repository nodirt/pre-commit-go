// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Globals

var goDirsCacheLock sync.Mutex
var goDirsCache map[bool][]string

var relToGOPATHLock sync.Mutex
var relToGOPATHCache = map[string]string{}

// TODO(maruel): Reimplement this in go instead of processing it in bash?
var preCommitHook = []byte(`#!/bin/sh
# Copyright 2015 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# pre-commit git hook to runs presubmit.py on the tree with unstaged changes
# removed.
#
# WARNING: This file was generated by tool "pre-commit-go"


# Redirect output to stderr.
exec 1>&2


run_checks() {
  # Ensure everything is either tracked or ignored. This is because git stash
  # doesn't stash untracked files.
  untracked="$(git ls-files --others --exclude-standard)"
  if [ "$untracked" != "" ]; then
    echo "This check refuses to run if there is an untracked file. Either track"
    echo "it or put it in the .gitignore or your global exclusion list:"
    echo "$untracked"
    return 1
  fi

  # Run the presubmit check.
  pre-commit-go run
  result=$?
  if [ $result != 0 ]; then
    return $result
  fi
}


if git rev-parse --verify HEAD >/dev/null 2>&1
then
  against=HEAD
else
  # Initial commit: diff against an empty tree object
  against=4b825dc642cb6eb9a060e54bf8d69288fbee4904
fi


# Use a precise "stash, run checks, unstash" to ensure that the check is
# properly run on the data in the index.
# Inspired from
# http://stackoverflow.com/questions/20479794/how-do-i-properly-git-stash-pop-in-pre-commit-hooks-to-get-a-clean-working-tree
# First, stash index and work dir, keeping only the to-be-committed changes in
# the working directory.
old_stash=$(git rev-parse -q --verify refs/stash)
git stash save -q --keep-index
new_stash=$(git rev-parse -q --verify refs/stash)

# If there were no changes (e.g., '--amend' or '--allow-empty') then nothing was
# stashed, and we should skip everything, including the tests themselves.
# (Presumably the tests passed on the previous commit, so there is no need to
# re-run them.)
if [ "$old_stash" = "$new_stash" ]; then
  exit 0
fi

run_checks
result=$?

# Restore changes.
git reset --hard -q && git stash apply --index -q && git stash drop -q
exit $result
`)

var helpText = `pre-commit-go: runs pre-commit checks on Go projects.

Supported commands are:
  help    - this page
  install - install the git commit hook as .git/hooks/pre-commit
  prereq  - install prerequisites: errcheck, golint, goimports, govet
  run     - run all checks

When executed without command, it does the equivalent of prereq, install then
run.

Supported flags are:
  -verbose

Supported checks:
- go build
- go test -race
- go test -cover
- gofmt -s
- goimports
- errcheck
- go tool vet
- golint

No check ever modify any file.
`

// Code

func readDirNames(dirname string) []string {
	f, err := os.Open(dirname)
	if err != nil {
		return nil
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	return names
}

// captureWd runs an executable from a directory returns the output, exit code
// and error if appropriate.
func captureWd(wd string, args ...string) (string, int, error) {
	exitCode := -1
	log.Printf("capture(%s)", args)
	c := exec.Command(args[0], args[1:]...)
	if wd != "" {
		c.Dir = wd
	}
	out, err := c.CombinedOutput()
	if c.ProcessState != nil {
		if waitStatus, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok {
			exitCode = waitStatus.ExitStatus()
			if exitCode != 0 {
				err = nil
			}
		}
	}
	// TODO(maruel): Handle code page on Windows.
	return string(out), exitCode, err
}

// capture runs an executable and returns the output, exit code and error if
// appropriate.
func capture(args ...string) (string, int, error) {
	return captureWd("", args...)
}

// captureAbs returns an absolute path of whatever a git command returned.
func captureAbs(args ...string) (string, error) {
	out, code, _ := capture(args...)
	if code != 0 {
		return "", fmt.Errorf("failed to run \"%s\"", strings.Join(args, " "))
	}
	path, err := filepath.Abs(strings.TrimSpace(out))
	log.Printf("captureAbs(%s) = %s", args, path)
	return path, err
}

// reverse reverses a string.
func reverse(s string) string {
	n := len(s)
	runes := make([]rune, n)
	for _, rune := range s {
		n--
		runes[n] = rune
	}
	return string(runes[n:])
}

func rsplitn(s, sep string, n int) []string {
	items := strings.SplitN(reverse(s), sep, n)
	l := len(items)
	for i := 0; i < l/2; i++ {
		j := l - i - 1
		items[i], items[j] = reverse(items[j]), reverse(items[i])
	}
	if l&1 != 0 {
		i := l / 2
		items[i] = reverse(items[i])
	}
	return items
}

// goDirs returns the list of directories with '*.go' files or '*_test.go'
// files, depending on value of 'tests'.
func goDirs(tests bool) []string {
	goDirsCacheLock.Lock()
	defer goDirsCacheLock.Unlock()
	if goDirsCache != nil {
		return goDirsCache[tests]
	}
	root, _ := os.Getwd()
	if stat, err := os.Stat(root); err != nil || !stat.IsDir() {
		panic("internal failure")
	}

	dirsSourceFound := map[string]bool{}
	dirsTestsFound := map[string]bool{}
	var recurse func(dir string)
	recurse = func(dir string) {
		for _, f := range readDirNames(dir) {
			if f[0] == '.' || f[0] == '_' {
				continue
			}
			p := filepath.Join(dir, f)
			stat, err := os.Stat(p)
			if err != nil {
				continue
			}
			if stat.IsDir() {
				recurse(p)
			} else {
				if strings.HasSuffix(p, "_test.go") {
					dirsTestsFound[dir] = true
				} else if strings.HasSuffix(p, ".go") {
					dirsSourceFound[dir] = true
				}
			}
		}
	}
	recurse(root)
	goDirsCache = map[bool][]string{
		false: make([]string, 0, len(dirsSourceFound)),
		true:  make([]string, 0, len(dirsTestsFound)),
	}
	for d := range dirsSourceFound {
		goDirsCache[false] = append(goDirsCache[false], d)
	}
	for d := range dirsTestsFound {
		goDirsCache[true] = append(goDirsCache[true], d)
	}
	sort.Strings(goDirsCache[false])
	sort.Strings(goDirsCache[true])
	//log.Printf("goDirs() = %v", goDirsCache)
	return goDirsCache[tests]
}

// relToGOPATH returns the path relative to $GOPATH/src.
func relToGOPATH(p string) (string, error) {
	relToGOPATHLock.Lock()
	defer relToGOPATHLock.Unlock()
	if rel, ok := relToGOPATHCache[p]; ok {
		return rel, nil
	}
	for _, gopath := range filepath.SplitList(os.Getenv("GOPATH")) {
		if len(gopath) == 0 {
			continue
		}
		srcRoot := filepath.Join(gopath, "src")
		// TODO(maruel): Also check filepath.EvalSymlinks()
		// TODO(maruel): Accept case-insensitivity.
		if !strings.HasPrefix(p, srcRoot) {
			continue
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return "", fmt.Errorf("failed to find relative path from %s to %s", srcRoot, p)
		}
		relToGOPATHCache[p] = rel
		//log.Printf("relToGOPATH(%s) = %s", p, rel)
		return rel, err
	}
	return "", fmt.Errorf("failed to find GOPATH relative directory for %s", p)
}

// Checks.

// build builds everything inside the current directory via 'go build ./...'.
func build(tags string) error {
	args := []string{"go", "build"}
	if len(tags) != 0 {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./...")
	out, _, err := capture(args...)
	if len(out) != 0 {
		return fmt.Errorf("%s failed: %s", strings.Join(args, " "), out)
	}
	if err != nil {
		return fmt.Errorf("%s failed: %s", strings.Join(args, " "), err.Error())
	}
	return nil
}

// runTests runs all tests with race detector.
func runTests(tags string) error {
	// Add tests manually instead of using './...'. The reason is that it permits
	// running all the tests concurrently, which saves a lot of time when there's
	// many packages.
	var wg sync.WaitGroup
	testDirs := goDirs(true)
	errs := make(chan error, len(testDirs))
	for _, t := range testDirs {
		wg.Add(1)
		go func(testDir string) {
			defer wg.Done()
			rel, err := relToGOPATH(testDir)
			if err != nil {
				errs <- err
				return
			}
			args := []string{"go", "test", "-race", "-v", rel}
			out, exitCode, _ := capture(args...)
			if exitCode != 0 {
				errs <- fmt.Errorf("%s failed:\n%s", strings.Join(args, " "), out)
			}
		}(t)
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

// runCoverage runs all tests with coverage.
func runCoverage() error {
	pkgRoot, _ := os.Getwd()
	pkg, err := relToGOPATH(pkgRoot)
	if err != nil {
		return err
	}
	testDirs := goDirs(true)
	if len(testDirs) == 0 {
		return nil
	}

	tmpDir, err := ioutil.TempDir("", "pre-commit-go")
	if err != nil {
		return err
	}
	defer func() {
		// TODO(maruel): Handle error.
		_ = os.RemoveAll(tmpDir)
	}()

	var wg sync.WaitGroup
	errs := make(chan error, len(testDirs))
	for i, t := range testDirs {
		wg.Add(1)
		go func(index int, testDir string) {
			defer wg.Done()
			args := []string{
				"go", "test", "-v", "-covermode=count", "-coverpkg", pkg + "/...",
				"-coverprofile=" + filepath.Join(tmpDir, fmt.Sprintf("test%d.cov", index)),
			}
			out, exitCode, _ := captureWd(testDir, args...)
			if exitCode != 0 {
				errs <- fmt.Errorf("%s %s failed:\n%s", strings.Join(args, " "), testDir, out)
			}
		}(i, t)
	}
	wg.Wait()

	// Merge the profiles. Sums all the counts.
	// Format is "file.go:XX.YY,ZZ.II J K"
	// J is number of statements, K is count.
	files, err := filepath.Glob(filepath.Join(tmpDir, "test*.cov"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		select {
		case err := <-errs:
			return err
		default:
			return errors.New("no coverage found")
		}
	}

	counts := map[string]int{}
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		s := bufio.NewScanner(f)
		// Strip the first line.
		s.Scan()
		count := 0
		for s.Scan() {
			items := rsplitn(s.Text(), " ", 2)
			count, err = strconv.Atoi(items[1])
			if err != nil {
				break
			}
			counts[items[0]] += int(count)
		}
		f.Close()
		if err != nil {
			return err
		}
	}

	profilePath := filepath.Join(tmpDir, "profile.cov")
	f, err := os.Create(profilePath)
	if err != nil {
		return err
	}
	stms := make([]string, 0, len(counts))
	for k := range counts {
		stms = append(stms, k)
	}
	sort.Strings(stms)
	_, _ = io.WriteString(f, "mode: count\n")
	for _, stm := range stms {
		fmt.Fprintf(f, "%s %d\n", stm, counts[stm])
	}
	f.Close()

	out := ""
	if len(os.Getenv("TRAVIS_JOB_ID")) != 0 {
		// Make sure to have registered to https://coveralls.io first!
		out, _, err = capture("goveralls", "-coverprofile", profilePath)
		fmt.Printf("%s", out)
	} else {
		out, _, err = capture("go", "tool", "cover", "-func", profilePath)
		type fn struct {
			loc  string
			name string
		}
		coverage := map[fn]float64{}
		var total float64
		for i, line := range strings.Split(out, "\n") {
			if i == 0 || len(line) == 0 {
				// First or last line.
				continue
			}
			items := strings.SplitN(line, "\t", 2)
			loc := items[0]
			if len(items) == 1 {
				panic(fmt.Sprintf("%#v %#v", line, items))
			}
			items = strings.SplitN(strings.TrimLeft(items[1], "\t"), "\t", 2)
			name := items[0]
			percentStr := strings.TrimLeft(items[1], "\t")
			percent, err := strconv.ParseFloat(percentStr[:len(percentStr)-1], 64)
			if err != nil {
				panic("internal failure")
			}
			if loc == "total:" {
				total = percent
			} else {
				coverage[fn{loc, name}] = percent
			}
		}
		// TODO(maruel): User configurable.
		if total < 20. {
			partial := 0
			for _, percent := range coverage {
				// TODO(maruel): User configurable.
				if percent < 95. {
					partial++
				}
			}
			err = fmt.Errorf("code coverage: %3.1f%%; %d untested functions", total, partial)
		}
	}

	if err == nil {
		select {
		case err = <-errs:
		default:
		}
	}
	return err
}

// errcheck runs errcheck on all directories containing .go files.
func errcheck() error {
	// TODO(maruel): I don't know what happened around Oct 2014 but errcheck
	// became super slow.
	dirs := goDirs(false)
	args := make([]string, 0, len(dirs)+2)
	args = append(args, "errcheck", "-ignore", "Close")
	for _, d := range dirs {
		rel, err := relToGOPATH(d)
		if err != nil {
			return err
		}
		args = append(args, rel)
	}
	// TODO(maruel): Add configurable -ignore like
	// "Close|Write.*|Flush|Seek|Read.*"
	out, _, err := capture(args...)
	if len(out) != 0 {
		return fmt.Errorf("%s failed:\n%s", strings.Join(args, " "), out)
	}
	if err != nil {
		return fmt.Errorf("%s failed: %s", strings.Join(args, " "), err)
	}
	return nil
}

// gofmt runs gofmt in check mode with code simplification enabled.
func gofmt() error {
	// TODO(maruel): Likely always redundant with goimports except for '-s'.
	// gofmt doesn't return non-zero even if some files need to be updated.
	out, _, err := capture("gofmt", "-l", "-s", ".")
	if len(out) != 0 {
		return fmt.Errorf("these files are improperly formmatted, please run: gofmt -w -s .\n%s", out)
	}
	if err != nil {
		return fmt.Errorf("gofmt -l -s . failed: %s", err)
	}
	return nil
}

// runs goimports in check mode.
func goimports() error {
	// goimports doesn't return non-zero even if some files need to be updated.
	out, _, err := capture("goimports", "-l", ".")
	if len(out) != 0 {
		return fmt.Errorf("these files are improperly formmatted, please run: goimports -w .\n%s", out)
	}
	if err != nil {
		return fmt.Errorf("goimports -w . failed: %s", err)
	}
	return nil
}

// golint runs golint.
// There starts the cheezy part that may return false positives. I'm sorry
// David.
func golint() error {
	// golint doesn't return non-zero ever.
	out, _, _ := capture("golint", "./...")
	// TODO(maruel): Make blacklist/whitelist user configurable.
	result := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		//if strings.Contains(line, ": don't use underscores in Go names; ") {
		//	continue
		//}
		result = append(result, line)
	}
	if len(result) == 0 {
		return errors.New(strings.Join(result, "\n"))
	}
	return nil
}

// govet runs "go tool vet".
func govet() error {
	// govet is very noisy about "composite literal uses unkeyed fields" which
	// cannot be turned off so strip these and ignore the return code.
	out, _, _ := capture("go", "tool", "vet", "-all", ".")
	// TODO(maruel): Make blacklist/whitelist user configurable.
	result := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(line, " composite literal uses unkeyed fields") {
			continue
		}
		result = append(result, line)
	}
	if len(result) == 0 {
		return errors.New(strings.Join(result, "\n"))
	}
	return nil
}

// Commands.

func installPrereq() error {
	type S struct {
		cmd      []string // Command to print the help page
		exitCode int      // Exit code when running help
		url      string   // URL to fetch the package
	}
	toInstall := []S{
		{[]string{"errcheck", "-h"}, 2, "github.com/kisielk/errcheck"},
		{[]string{"go", "tool", "cover", "-h"}, 1, "golang.org/x/tools/cmd/cover"},
		{[]string{"go", "tool", "vet", "-h"}, 1, "golang.org/x/tools/cmd/vet"},
		{[]string{"gocov", "-h"}, 2, "github.com/axw/gocov/gocov"},
		{[]string{"goimports", "-h"}, 2, "golang.org/x/tools/cmd/goimports"},
		{[]string{"golint", "-h"}, 2, "github.com/golang/lint/golint"},
		{[]string{"goveralls", "-h"}, 2, "github.com/mattn/goveralls"},
	}
	var wg sync.WaitGroup
	c := make(chan string, len(toInstall))
	for _, i := range toInstall {
		wg.Add(1)
		go func(item S) {
			defer wg.Done()
			_, exitCode, _ := capture(item.cmd...)
			if exitCode != item.exitCode {
				c <- item.url
			}
		}(i)
	}
	wg.Wait()
	urls := []string{}
	loop := true
	for loop {
		select {
		case url := <-c:
			urls = append(urls, url)
		default:
			loop = false
		}
	}
	sort.Strings(urls)
	if len(urls) != 0 {
		fmt.Printf("Installing:\n")
		for _, url := range urls {
			fmt.Printf("  %s\n", url)
		}
		out, _, err := capture(append([]string{"go", "get", "-u"}, urls...)...)
		if len(out) != 0 {
			return fmt.Errorf("prerequisites installation failed: %s", out)
		}
		if err != nil {
			return fmt.Errorf("prerequisites installation failed: %s", err)
		}
	}
	return nil
}

func install() error {
	if err := installPrereq(); err != nil {
		return err
	}
	gitDir, err := captureAbs("git", "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("failed to find .git dir: %s", err)
	}
	// Always remove "pre-commit" first if it exists, in case it's a symlink.
	p := filepath.Join(gitDir, "hooks", "pre-commit")
	_ = os.Remove(p)
	err = ioutil.WriteFile(p, preCommitHook, 0766)
	log.Printf("installation done")
	return err
}

func run() error {
	start := time.Now()
	type Check struct {
		name string
		fn   func() error
	}
	// TODO(maruel): User configurable.
	tags := ""
	// TODO(maruel): User configurable.
	checks := []Check{
		{"build", func() error { return build(tags) }},
		{"test", func() error { return runTests(tags) }},
		{"gofmt", gofmt},
		{"goimports", goimports},
		{"errcheck", errcheck},
		{"govet", govet},
		{"golint", golint},
		{"coverage", runCoverage},
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(checks))
	for _, c := range checks {
		wg.Add(1)
		go func(check Check) {
			defer wg.Done()
			log.Printf("%s...", check.name)
			start := time.Now()
			err := check.fn()
			duration := time.Now().Sub(start)
			log.Printf("... %s in %1.2fs", check.name, duration.Seconds())
			if err != nil {
				errs <- err
			}
			// A check that took 2 minutes is a check that failed.
			// TODO(maruel): User configurable.
			if duration > 120*time.Second {
				errs <- fmt.Errorf("check %s took %1.2fs", check.name, duration.Seconds())
			}
		}(c)
	}
	wg.Wait()

	var err error
	for {
		select {
		case err = <-errs:
			fmt.Printf("%s\n", err)
		default:
			if err != nil {
				duration := time.Now().Sub(start)
				return fmt.Errorf("checks failed in %1.2fs", duration.Seconds())
			}
			return err
		}
	}
}

func mainImpl() error {
	cmd := ""
	if len(os.Args) == 1 {
		cmd = "installRun"
	} else {
		cmd = os.Args[1]
		copy(os.Args[1:], os.Args[2:])
		os.Args = os.Args[:len(os.Args)-1]
	}
	verbose := flag.Bool("verbose", false, "verbose")
	flag.Parse()

	log.SetFlags(log.Lmicroseconds)
	if !*verbose {
		log.SetOutput(ioutil.Discard)
	}

	gitRoot, err := captureAbs("git", "rev-parse", "--show-cdup")
	if err != nil {
		return fmt.Errorf("failed to find git checkout root")
	}
	if err := os.Chdir(gitRoot); err != nil {
		return fmt.Errorf("failed to chdir to git checkout root: %s", err)
	}

	if cmd == "help" || cmd == "-help" || cmd == "-h" {
		fmt.Printf(helpText)
		return nil
	}
	if cmd == "install" || cmd == "i" {
		return install()
	}
	if cmd == "installRun" {
		if err := install(); err != nil {
			return err
		}
		return run()
	}
	if cmd == "prereq" || cmd == "p" {
		return installPrereq()
	}
	if cmd == "run" || cmd == "r" {
		return run()
	}
	return errors.New("unknown command")
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "pre-commit-go: %s\n", err)
		os.Exit(1)
	}
}
