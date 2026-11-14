//go:build windows

package main

import "os"

// rawWrite does nothing on Windows, which has no descriptor 1: a handle
// copied before main is the one way past the guard there (design 5.3).
func rawWrite(string) {}

// fd1Open reports true: Windows has no descriptor 1 to lose.
func fd1Open() bool { return true }

// descriptor returns -1: a Windows handle is no descriptor.
func descriptor(*os.File) int { return -1 }
