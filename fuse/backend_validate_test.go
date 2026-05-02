// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"strings"
	"testing"
)

func TestValidateBackend(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		goos    string
		backend string
		wantErr string // substring expected in error; "" means no error
	}{
		{"empty backend is always ok (linux)", "linux", "", ""},
		{"empty backend is always ok (darwin)", "darwin", "", ""},
		{"fskit on darwin is ok", "darwin", "fskit", ""},
		{"fskit on linux is rejected", "linux", "fskit", "only supported on darwin"},
		{"fskit on freebsd is rejected", "freebsd", "fskit", "only supported on darwin"},
		{"unknown backend on darwin is ok at this layer", "darwin", "bogus", ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateBackend(tt.goos, tt.backend)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("got nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
