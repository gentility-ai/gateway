package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// spool keeps one file per unsent record: written to a temporary name,
// fsynced, renamed, and the directory fsynced, so a record either survives a
// crash whole or was never acknowledged. Sending deletes the file.
type spool struct{ dir string }

func (s *spool) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", errors.New("request id is not a file name")
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s *spool) append(r record) error {
	target, err := s.path(r.RequestID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeDurably(target, raw)
}

// writeDurably replaces a file whole or not at all, and survives a crash once
// it returns.
func writeDurably(target string, raw []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func (s *spool) unsent(limit int) ([]record, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	records := []record{}
	for _, name := range names {
		if len(records) == limit {
			break
		}
		file := filepath.Join(s.dir, name)
		raw, err := os.ReadFile(file)
		var r record
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		// A file that cannot be read or parsed is set aside, never resent,
		// so one bad file cannot hold back every record after it.
		if err != nil {
			log.Printf("gentility-usage: setting aside %s: %v", name, err)
			if err := os.Rename(file, file+".bad"); err != nil {
				return nil, err
			}
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

func (s *spool) sent(ids []string) error {
	for _, id := range ids {
		target, err := s.path(id)
		if err != nil {
			continue
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
