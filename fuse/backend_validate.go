// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import "fmt"

// validateBackend checks that MountOptions.Backend is compatible with the
// build's target OS. The "fskit" backend (and any future named backends)
// are darwin-only; on other platforms a non-empty Backend yields a clear
// error from NewServer rather than a confusing downstream failure.
func validateBackend(goos, backend string) error {
	if backend == "" {
		return nil
	}
	if goos != "darwin" {
		return fmt.Errorf("MountOptions.Backend = %q is only supported on darwin (current GOOS: %s)", backend, goos)
	}
	return nil
}
