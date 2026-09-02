/*
Copyright 2026 The gateway-api-openstack Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWriteAndLoadRuntimeUsesStrictPrivateFile(t *testing.T) {
	config := validRuntime(t)
	path, err := WriteRuntime(t.TempDir(), config)
	if err != nil {
		t.Fatalf("WriteRuntime() error = %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("runtime path = %q, want absolute", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime mode = %#o, want 0600", info.Mode().Perm())
	}
	loaded, err := LoadRuntime(path)
	if err != nil {
		t.Fatalf("LoadRuntime() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, config) {
		t.Fatalf("loaded runtime = %#v, want %#v", loaded, config)
	}
}

func TestLoadRuntimeRejectsUnsafeOrNonStrictDocuments(t *testing.T) {
	config := validRuntime(t)
	valid, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		contents []byte
		mode     os.FileMode
		want     string
	}{
		{name: "unknown field", contents: append(append([]byte(nil), valid[:len(valid)-1]...), []byte(`,"unknown":true}`)...), mode: 0o600, want: "unknown field"},
		{name: "trailing value", contents: append(append([]byte(nil), valid...), []byte("\n{}\n")...), mode: 0o600, want: "trailing JSON"},
		{name: "public permissions", contents: valid, mode: 0o644, want: "group or other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.json")
			if err := os.WriteFile(path, test.contents, test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRuntime(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadRuntime() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadRuntimeRejectsSymlinkAndChangedIdentity(t *testing.T) {
	config := validRuntime(t)
	directory := t.TempDir()
	path, err := WriteRuntime(directory, config)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "runtime-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntime(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("LoadRuntime(symlink) error = %v", err)
	}

	config.Namespace = "gateway-api-openstack-e2e-another-run"
	if _, err := WriteRuntime(directory, config); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("WriteRuntime(changed identity) error = %v", err)
	}
}

func validRuntime(t *testing.T) Runtime {
	t.Helper()
	config, err := Resolve(validFile(), ResolveOptions{RepositoryRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return config
}
