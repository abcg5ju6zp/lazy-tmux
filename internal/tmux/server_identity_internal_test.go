package tmux

import (
	"os"
	"strconv"
	"testing"
)

// These tests mutate process environment, so they must not run in parallel.

func defaultSocketFor(dir string) string {
	return dir + "/tmux-" + strconv.Itoa(os.Getuid()) + "/default"
}

func TestServerIdentityPrefersLiveSocket(t *testing.T) {
	t.Setenv("TMUX", "/from/env/default,1234,0")
	t.Setenv("TMUX_TMPDIR", "/should/not/be/used")

	client := NewClientWithRunner("tmux", stdoutRunner{out: " /srv/tmux-sockets/work \n"})

	if got := client.ServerIdentity(); got != "/srv/tmux-sockets/work" {
		t.Fatalf("live socket must win, got %q", got)
	}
}

func TestServerIdentityFallsBackToTMuxEnv(t *testing.T) {
	t.Setenv("TMUX", "/from/env/default,4321,1")

	client := NewClientWithRunner("tmux", stdoutRunner{err: errFakeNoServer})

	if got := client.ServerIdentity(); got != "/from/env/default" {
		t.Fatalf("expected $TMUX socket, got %q", got)
	}
}

func TestServerIdentityFallsBackToDefaultPath(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", "/tmp/lazy")

	client := NewClientWithRunner("tmux", stdoutRunner{err: errFakeNoServer})

	if got, want := client.ServerIdentity(), defaultSocketFor("/tmp/lazy"); got != want {
		t.Fatalf("derived default socket = %q, want %q", got, want)
	}
}

func TestDefaultSocketPathIgnoresGenericTmpdir(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", "")
	t.Setenv("TMPDIR", "/var/folders/does-not-matter")

	if got, want := DefaultSocketPath(), defaultSocketFor("/tmp"); got != want {
		t.Fatalf("default path = %q, want %q", got, want)
	}
}

func TestSocketPathMatchesServerIdentity(t *testing.T) {
	t.Setenv("TMUX", "/from/env/default,1,0")

	client := NewClientWithRunner("tmux", stdoutRunner{out: "/srv/tmux-sockets/work"})

	if client.SocketPath() != client.ServerIdentity() {
		t.Fatal("SocketPath and ServerIdentity must agree for the daemon lock")
	}
}
