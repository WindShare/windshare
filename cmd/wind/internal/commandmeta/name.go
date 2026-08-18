// Package commandmeta defines the stable public identity of the native CLI.
package commandmeta

// Name stays independent of os.Args[0] so help and diagnostics remain stable
// when callers invoke the executable through an absolute path or a symlink.
const Name = "wind"
