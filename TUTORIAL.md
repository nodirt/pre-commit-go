Tutorial
========

Also see the full [configuration](CONFIGURATION.md) page.


Ignoring files
--------------

By default, the following pattern is used which skips files mapped in by
[godep](https://github.com/tools/godep) (.e.g. *Godeps/_workspace*)
automatically:

    ignore_patterns:
    - .*
    - _*

Any other glob can be added.


Forcing update for clients
--------------------------

`pre-commit-go` refuses to load a file if its version is less than what is
specified by `min_version`. So to force all the contributors to upgrade to the
current version, remove the `min_version` line from your `pre-commit-go.yml`
file then run:

    pre-commit-go writeconfig

This command will set `min_version` to the current version.


Build on pre-commit only
------------------------

A trivial configuration that would build everything on commit only would look
like:

    modes:
      pre-commit:
        check:
        - build:
          - extra_args: []

This is useful for small projects that do not contain tests. It ensures that at
least the code compiles before commit.


Test with -race
---------------

To run tests with `-race` but only short test to reduce the amount of time taken
locally, and only on push to keep commits fast, use:

    modes:
      pre-push:
        check:
        - test:
          - extra_args:
            - -race
            - -short

This permits to get an assurance that tests expose any race condition as found
by the [race detector](http://blog.golang.org/race-detector). Then in mode
`continous-integration`, the same check can be used but without the `-short`
flag.

Tests that shouldn't be run ever in -race mode because they are unacceptably
slow can be migrated in a file with the [following
statement](http://golang.org/doc/articles/race_detector.html#Excluding_Tests):

    // +build: !race

When a test is looping over itself, it is also possible to migrate the constant
to a single source file so the constant can be fine tuned when using or not the
race detector.