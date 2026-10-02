//go:build !unix

package main

import "os"

func lockFile(path string) (*os.File, error) { return nil, nil }
