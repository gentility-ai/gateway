package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpoolSurvivesARestartAndForgetsWhatWasSent(t *testing.T) {
	dir := t.TempDir()
	s := &spool{dir: dir}
	for _, id := range []string{"a", "b"} {
		if err := s.append(record{RequestID: id, KeyHash: "h"}); err != nil {
			t.Fatal(err)
		}
	}
	again := &spool{dir: dir}
	unsent, err := again.unsent(10)
	if err != nil || len(unsent) != 2 {
		t.Fatalf("unsent %v %v", unsent, err)
	}
	if err := again.sent([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	unsent, _ = again.unsent(10)
	if len(unsent) != 1 || unsent[0].RequestID != "b" {
		t.Fatalf("left %v", unsent)
	}
}

func TestSpoolRefusesAPathInARequestID(t *testing.T) {
	s := &spool{dir: t.TempDir()}
	if err := s.append(record{RequestID: "../x", KeyHash: "h"}); err == nil {
		t.Fatal("a request id is a file name, never a path")
	}
}

func TestSpoolSetsAsideAFileItCannotRead(t *testing.T) {
	dir := t.TempDir()
	s := &spool{dir: dir}
	if err := s.append(record{RequestID: "good", KeyHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsent, err := s.unsent(10)
	if err != nil || len(unsent) != 1 || unsent[0].RequestID != "good" {
		t.Fatalf("unsent %v %v", unsent, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.json.bad")); err != nil {
		t.Fatal("the unreadable file is set aside as .bad")
	}
}
