// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugin

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// The plugin catalog (registry/ in this repository) publishes plugins as
// ZIP archives with a plugin.json. The server installs them into its
// plugins directory and the speakers into theirs, both with
// [InstallArchive].

const (
	// MaxArchive is the archive limit of the catalog.
	MaxArchive = 64 << 20
	// MaxUnpacked caps what an archive may unpack to.
	MaxUnpacked = 512 << 20
	// CatalogMark is the file in a plugin directory that records where the
	// plugin came from. Only such plugins are replaced and removed.
	CatalogMark = ".catalog.json"
)

// CatalogInstall is what [CatalogMark] records.
type CatalogInstall struct {
	ProjectID   string    `json:"project_id"`
	Slug        string    `json:"slug"`
	Version     string    `json:"version"`
	SHA512      string    `json:"sha512"`
	Catalog     string    `json:"catalog"`
	InstalledAt time.Time `json:"installed_at"`
}

// ErrNotCatalog is returned for a plugin that was not installed from the
// catalog.
var ErrNotCatalog = errors.New("плагин установлен не из каталога, удалите его папку вручную")

// ReadCatalogMark reads the [CatalogMark] of a plugin directory.
func ReadCatalogMark(dir string) (CatalogInstall, bool) {
	var mark CatalogInstall
	b, err := os.ReadFile(filepath.Join(dir, CatalogMark))
	if err != nil || json.Unmarshal(b, &mark) != nil {
		return mark, false
	}
	return mark, true
}

// InstallArchive unpacks a plugin archive into root/<id>, replacing an
// older version, and records mark in it. check may refuse the manifest
// before anything is replaced. rescan, if set, makes the plugin manager
// forget the old version before the new one appears and then pick up the
// new one. The caller serializes installs into one root.
func InstallArchive(root string, archive []byte, mark CatalogInstall, check func(Manifest) error, rescan func()) (Manifest, error) {
	if rescan == nil {
		rescan = func() {}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Manifest{}, err
	}
	tmp, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil {
		return Manifest{}, err
	}
	if err := Unpack(archive, tmp); err != nil {
		return Manifest{}, err
	}
	man, err := Load(tmp)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin.json в архиве: %w", err)
	}
	if check != nil {
		if err := check(man); err != nil {
			return Manifest{}, err
		}
	}
	mark.Slug = man.ID
	if mark.InstalledAt.IsZero() {
		mark.InstalledAt = time.Now().UTC()
	}
	b, _ := json.MarshalIndent(mark, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, CatalogMark), b, 0o644); err != nil {
		return Manifest{}, err
	}

	dst := filepath.Join(root, man.ID)
	var old string
	if _, err := os.Lstat(dst); err == nil {
		// Stop the running version first: the manager keeps the manifest
		// of a plugin it knows, so it must forget it before the new one.
		old = filepath.Join(root, ".old-"+man.ID+"-"+fmt.Sprint(time.Now().UnixNano()))
		if err := os.Rename(dst, old); err != nil {
			return Manifest{}, fmt.Errorf("не удалось заменить %s: %w", dst, err)
		}
		rescan()
	}
	if err := os.Rename(tmp, dst); err != nil {
		if old != "" {
			os.Rename(old, dst)
			rescan()
		}
		return Manifest{}, fmt.Errorf("не удалось установить в %s: %w", dst, err)
	}
	if old != "" {
		os.RemoveAll(old)
	}
	rescan()
	man.Dir = dst
	return man, nil
}

// RemoveCatalog removes the plugin root/<id> if it was installed from the
// catalog: a directory with a [CatalogMark], not a symlink to the user's
// files. It returns an error matching os.ErrNotExist without such a
// directory and [ErrNotCatalog] for other plugins.
func RemoveCatalog(root, id string, rescan func()) error {
	if id == "" || id != filepath.Base(id) || strings.HasPrefix(id, ".") {
		return os.ErrNotExist
	}
	dir := filepath.Join(root, id)
	st, err := os.Lstat(dir)
	switch {
	case err != nil:
		return err
	case !st.IsDir():
		return ErrNotCatalog
	}
	if _, ok := ReadCatalogMark(dir); !ok {
		return ErrNotCatalog
	}
	trash := filepath.Join(root, ".removed-"+id+"-"+fmt.Sprint(time.Now().UnixNano()))
	if err := os.Rename(dir, trash); err != nil {
		return err
	}
	if rescan != nil {
		rescan()
	}
	return os.RemoveAll(trash)
}

// Unpack unpacks a plugin ZIP into dir. plugin.json is at the root of the
// archive or in its only top-level directory, which is stripped. Entries
// that would leave dir, links and special files are refused, and so are
// archives unpacking to more than [MaxUnpacked].
func Unpack(archive []byte, dir string) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return errors.New("архив плагина не является ZIP-файлом")
	}
	prefix, err := archiveRoot(zr)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		rel, ok := strings.CutPrefix(name, prefix)
		if !ok || rel == "" {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(rel)), 0o755); err != nil {
				return err
			}
			continue
		case !mode.IsRegular():
			return fmt.Errorf("в архиве плагина недопустимый файл %q", f.Name)
		}
		total += int64(f.UncompressedSize64)
		if f.UncompressedSize64 > MaxUnpacked || total > MaxUnpacked {
			return errors.New("архив плагина распаковывается больше чем в 512 МБ")
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if mode.Perm()&0o111 != 0 {
			perm = 0o755
		}
		if err := writeZipFile(f, dst, perm); err != nil {
			return err
		}
	}
	return nil
}

// archiveRoot checks the paths of an archive and returns the prefix of the
// plugin directory in it: "" or "name/".
func archiveRoot(zr *zip.Reader) (string, error) {
	tops := map[string]bool{}
	atRoot, nested := false, ""
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		clean := path.Clean(name)
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") ||
			clean != strings.TrimSuffix(name, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
			return "", fmt.Errorf("в архиве плагина недопустимый путь %q", f.Name)
		}
		top, _, _ := strings.Cut(clean, "/")
		tops[top] = true
		switch {
		case clean == ManifestFile:
			atRoot = true
		case strings.Count(clean, "/") == 1 && path.Base(clean) == ManifestFile:
			nested = top + "/"
		}
	}
	switch {
	case atRoot:
		return "", nil
	case nested != "" && len(tops) == 1:
		return nested, nil
	}
	return "", errors.New("в архиве плагина нет plugin.json")
}

func writeZipFile(f *zip.File, dst string, perm os.FileMode) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("архив плагина: %w", err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, int64(f.UncompressedSize64)+1)); err != nil {
		out.Close()
		return fmt.Errorf("архив плагина: %w", err)
	}
	return out.Close()
}
