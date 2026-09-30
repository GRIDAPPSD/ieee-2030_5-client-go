package gridlabd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyPeerPID proves the check both ways: it accepts the real peer
// (this test process, since both ends of the pair are dialed from here)
// and refuses a pid that is not it. The false case is what the reviewed
// exploit needed: a foreign process bound the socket first, and nothing
// compared who actually answered against who was expected to.
func TestVerifyPeerPID(t *testing.T) {
	dir := shortSockDir(t)
	sockPath := filepath.Join(dir, "peer.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	clientConn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()
	serverConn := <-accepted
	defer serverConn.Close()

	if err := verifyPeerPID(clientConn, os.Getpid()); err != nil {
		t.Errorf("verifyPeerPID(correct pid): %v", err)
	}
	if err := verifyPeerPID(clientConn, os.Getpid()+123456); err == nil {
		t.Error("verifyPeerPID(wrong pid): want error, got nil")
	}
}

func TestRemoveIfSocket_RemovesASocket(t *testing.T) {
	dir := shortSockDir(t)
	sockPath := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close() // leaves the socket file on disk, as an unclean exit would

	removeIfSocket(sockPath)
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after removeIfSocket: err=%v", err)
	}
}

// TestRemoveIfSocket_RefusesNonSocket is the finding: startOnce's stale-
// path cleanup must not delete a regular file (or anything else) planted
// at the socket path, only a genuine socket.
func TestRemoveIfSocket_RefusesNonSocket(t *testing.T) {
	dir := shortSockDir(t)
	planted := filepath.Join(dir, "a.sock")
	if err := os.WriteFile(planted, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	removeIfSocket(planted)
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("planted regular file was removed (or is now inaccessible): %v", err)
	}
}
