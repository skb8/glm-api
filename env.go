package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type envFile struct {
	path  string
	lines []string
	dirty bool
}

func readEnv(path string) (*envFile, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	env := &envFile{path: path}
	if len(data) > 0 {
		env.lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	return env, nil
}

func (e *envFile) get(key string) string {
	var value string
	for _, line := range e.lines {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == key {
			value = strings.Trim(strings.TrimSpace(v), "'\"")
		}
	}
	return value
}

func (e *envFile) set(key, value string) {
	if e.get(key) == value {
		return
	}
	prefix := key + "="
	for i := len(e.lines) - 1; i >= 0; i-- {
		if k, _, ok := strings.Cut(strings.TrimSpace(e.lines[i]), "="); ok && strings.TrimSpace(k) == key {
			e.lines[i] = prefix + value
			e.dirty = true
			return
		}
	}
	e.lines = append(e.lines, prefix+value)
	e.dirty = true
}

func (e *envFile) save() error {
	if !e.dirty {
		return os.Chmod(e.path, 0600)
	}
	file, err := os.CreateTemp(filepath.Dir(e.path), ".glm-api-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := fmt.Fprintln(file, strings.Join(e.lines, "\n"))
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), e.path); err != nil {
		return err
	}
	e.dirty = false
	return nil
}
