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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	sharedCacheDirName    = "env-cache"
	sharedCacheMaxEntries = 256
	sharedCacheMaxAge     = 30 * 24 * time.Hour
)

// SharedKeyInput collects every input that determines `vfox env` output for
// a session that carries no session-scope state. Session-scoped sessions
// (session config present, or output referencing the session shim dir) must
// never read from or write to the shared cache — the callers enforce that
// eligibility; this type only guarantees that any difference in any listed
// input yields a different key.
type SharedKeyInput struct {
	// RuntimeVersion invalidates the whole cache across vfox upgrades.
	RuntimeVersion string
	// ShellName is the output dialect (zsh/bash/...).
	ShellName string
	// Path is the raw $PATH the command was invoked with.
	Path string
	// GlobalConfig / ProjectConfig are ConfigStamp values ("" when the
	// scope has no config file).
	GlobalConfig  string
	ProjectConfig string
}

// ConfigStamp returns a stable "path\x00mtime" stamp for a config file, or
// "" when path is empty (scope absent). A stat failure is surfaced so the
// caller can opt out of sharing instead of computing a wrong key.
func ConfigStamp(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\x00%d", path, info.ModTime().Unix()), nil
}

// canonical serializes the input unambiguously; it is stored inside each
// entry and compared on lookup, so even a hash collision cannot serve a
// wrong output.
func (in SharedKeyInput) canonical() string {
	return strings.Join([]string{
		"version\x00" + in.RuntimeVersion,
		"shell\x00" + in.ShellName,
		"path\x00" + in.Path,
		"global\x00" + in.GlobalConfig,
		"project\x00" + in.ProjectConfig,
	}, "\x01")
}

// Hash returns the content-address of the input, used as the entry filename.
func (in SharedKeyInput) Hash() string {
	sum := sha256.Sum256([]byte(in.canonical()))
	return hex.EncodeToString(sum[:])
}

type sharedCacheEntry struct {
	Key       string `json:"key"`
	Output    string `json:"output"`
	CreatedAt int64  `json:"created_at"`
}

// SharedEnvCache is a machine-global, content-addressed cache of `vfox env`
// outputs, shared across shell sessions. It sits between the per-session
// env-state fast path and the full rebuild: a brand-new session whose inputs
// match an entry skips the expensive plugin evaluation entirely.
type SharedEnvCache struct {
	dir string
}

// NewSharedEnvCache returns the cache rooted under the vfox user home
// (~/.version-fox/env-cache).
func NewSharedEnvCache(vfoxUserHome string) *SharedEnvCache {
	return &SharedEnvCache{dir: filepath.Join(vfoxUserHome, sharedCacheDirName)}
}

func (c *SharedEnvCache) entryPath(in SharedKeyInput) string {
	return filepath.Join(c.dir, in.Hash()+".json")
}

// Lookup returns the cached output for the input, if present. A readable
// entry whose stored key does not byte-match the input is ignored. Hits
// touch the entry mtime so Prune can expire by last use.
func (c *SharedEnvCache) Lookup(in SharedKeyInput) (string, bool) {
	file := c.entryPath(in)
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	var entry sharedCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", false
	}
	if entry.Key != in.canonical() || entry.Output == "" {
		return "", false
	}
	now := time.Now()
	_ = os.Chtimes(file, now, now)
	return entry.Output, true
}

// Store writes the output for the input atomically (temp file + rename), so
// concurrent shells can never observe a partial entry.
func (c *SharedEnvCache) Store(in SharedKeyInput, output string) error {
	if err := os.MkdirAll(c.dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sharedCacheEntry{
		Key:       in.canonical(),
		Output:    output,
		CreatedAt: time.Now().Unix(),
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, c.entryPath(in))
}

// Prune removes entries unused for sharedCacheMaxAge and, if the cache still
// exceeds sharedCacheMaxEntries, the least recently used overflow. Leftover
// temp files from crashed writers are removed once they are a day old.
func (c *SharedEnvCache) Prune() {
	dirEntries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path  string
		mtime time.Time
	}
	var entries []fileInfo
	ageCutoff := time.Now().Add(-sharedCacheMaxAge)
	tmpCutoff := time.Now().Add(-24 * time.Hour)
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(c.dir, de.Name())
		if strings.HasPrefix(de.Name(), ".tmp-") {
			if info.ModTime().Before(tmpCutoff) {
				_ = os.Remove(full)
			}
			continue
		}
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		if info.ModTime().Before(ageCutoff) {
			_ = os.Remove(full)
			continue
		}
		entries = append(entries, fileInfo{path: full, mtime: info.ModTime()})
	}
	if len(entries) <= sharedCacheMaxEntries {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime.After(entries[j].mtime)
	})
	for _, e := range entries[sharedCacheMaxEntries:] {
		_ = os.Remove(e.path)
	}
}
