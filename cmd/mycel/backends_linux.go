package main

// Sandbox backends compiled into the Linux binary.
import (
	_ "github.com/richard-sen27/mycelium/internal/sandbox/firecracker"
	_ "github.com/richard-sen27/mycelium/internal/sandbox/gvisor"
)
