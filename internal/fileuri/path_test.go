package fileuri

import (
	"path/filepath"
	"testing"
)

func TestPathFromURIWindowsDrive(t *testing.T) {
	got, ok := pathFromURI("file:///D:/workspace/project%20one", true)
	if !ok {
		t.Fatal("file URI was not recognized")
	}
	want := filepath.FromSlash("D:/workspace/project one")
	if got != want {
		t.Fatalf("pathFromURI() = %q, want %q", got, want)
	}
}

func TestPathFromURIUnixKeepsLeadingSlash(t *testing.T) {
	got, ok := pathFromURI("file:///D:/workspace/project", false)
	if !ok {
		t.Fatal("file URI was not recognized")
	}
	want := filepath.FromSlash("/D:/workspace/project")
	if got != want {
		t.Fatalf("pathFromURI() = %q, want %q", got, want)
	}
}

func TestPathFromURIWindowsUNC(t *testing.T) {
	got, ok := pathFromURI("file://server/share/project", true)
	if !ok {
		t.Fatal("file URI was not recognized")
	}
	want := filepath.FromSlash("//server/share/project")
	if got != want {
		t.Fatalf("pathFromURI() = %q, want %q", got, want)
	}
}
