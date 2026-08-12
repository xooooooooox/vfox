/*
 *    Copyright 2026 Han Li and contributors
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package env

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleInput() SharedKeyInput {
	return SharedKeyInput{
		RuntimeVersion: "1.0.11",
		ShellName:      "zsh",
		Path:           "/usr/bin:/bin",
		GlobalConfig:   "/home/u/.version-fox/.tool-versions\x001700000000",
		ProjectConfig:  "",
	}
}

func TestSharedKeyHashStability(t *testing.T) {
	a, b := sampleInput(), sampleInput()
	if a.Hash() != b.Hash() {
		t.Fatal("identical inputs must produce identical hashes")
	}

	variants := []SharedKeyInput{
		func(in SharedKeyInput) SharedKeyInput { in.RuntimeVersion = "1.0.12"; return in }(sampleInput()),
		func(in SharedKeyInput) SharedKeyInput { in.ShellName = "bash"; return in }(sampleInput()),
		func(in SharedKeyInput) SharedKeyInput { in.Path = "/usr/bin:/bin:/opt"; return in }(sampleInput()),
		func(in SharedKeyInput) SharedKeyInput { in.GlobalConfig = ""; return in }(sampleInput()),
		func(in SharedKeyInput) SharedKeyInput {
			in.ProjectConfig = "/p/vfox.toml\x001700000001"
			return in
		}(sampleInput()),
	}
	seen := map[string]struct{}{a.Hash(): {}}
	for i, v := range variants {
		h := v.Hash()
		if _, dup := seen[h]; dup {
			t.Fatalf("variant %d did not change the hash", i)
		}
		seen[h] = struct{}{}
	}
}

func TestSharedCacheRoundTrip(t *testing.T) {
	cache := NewSharedEnvCache(t.TempDir())
	in := sampleInput()

	if _, ok := cache.Lookup(in); ok {
		t.Fatal("lookup on empty cache must miss")
	}
	if err := cache.Store(in, "export PATH=/x"); err != nil {
		t.Fatalf("store failed: %v", err)
	}
	out, ok := cache.Lookup(in)
	if !ok || out != "export PATH=/x" {
		t.Fatalf("lookup after store: ok=%v out=%q", ok, out)
	}

	other := sampleInput()
	other.Path = "/different"
	if _, ok := cache.Lookup(other); ok {
		t.Fatal("different input must miss")
	}
}

func TestSharedCacheRejectsCorruptAndMismatchedEntries(t *testing.T) {
	home := t.TempDir()
	cache := NewSharedEnvCache(home)
	in := sampleInput()

	if err := os.MkdirAll(cache.dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.entryPath(in), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Lookup(in); ok {
		t.Fatal("corrupt entry must miss")
	}

	// A file at the right address but recording a different key must miss.
	mismatch := sampleInput()
	mismatch.ShellName = "bash"
	if err := cache.Store(mismatch, "export A=1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cache.entryPath(mismatch), cache.entryPath(in)); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Lookup(in); ok {
		t.Fatal("entry with mismatched key must miss")
	}
}

func TestConfigStamp(t *testing.T) {
	if s, err := ConfigStamp(""); err != nil || s != "" {
		t.Fatalf("empty path: s=%q err=%v", s, err)
	}
	if _, err := ConfigStamp(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file must return an error")
	}

	f := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := ConfigStamp(f)
	if err != nil {
		t.Fatal(err)
	}
	newTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(f, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	after, err := ConfigStamp(f)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("mtime change must change the stamp")
	}
}

func TestSharedCachePrune(t *testing.T) {
	cache := NewSharedEnvCache(t.TempDir())

	// One stale entry, one stale temp file, then a full complement of fresh
	// entries exceeding the count cap.
	stale := sampleInput()
	stale.Path = "/stale"
	if err := cache.Store(stale, "old"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-sharedCacheMaxAge - time.Hour)
	if err := os.Chtimes(cache.entryPath(stale), old, old); err != nil {
		t.Fatal(err)
	}
	staleTmp := filepath.Join(cache.dir, ".tmp-crashed")
	if err := os.WriteFile(staleTmp, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleTmp, old, old); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < sharedCacheMaxEntries+10; i++ {
		in := sampleInput()
		in.Path = fmt.Sprintf("/fresh/%d", i)
		if err := cache.Store(in, "fresh"); err != nil {
			t.Fatal(err)
		}
	}

	cache.Prune()

	if _, err := os.Stat(cache.entryPath(stale)); !os.IsNotExist(err) {
		t.Fatal("stale entry must be pruned")
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatal("stale temp file must be pruned")
	}
	dirEntries, err := os.ReadDir(cache.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirEntries) > sharedCacheMaxEntries {
		t.Fatalf("count cap not enforced: %d entries remain", len(dirEntries))
	}
}
