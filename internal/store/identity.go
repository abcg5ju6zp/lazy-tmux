package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	serversDirName     = "servers"
	identityFileName   = "identity.json"
	legacyIndexName    = "legacy"
	identityReadLimit  = 4096
	maxNamespaceNameLn = 40
)

// LegacyServerID is the namespace of pre-namespace snapshots. They stay
// selectable after an upgrade but can never be proven live for a running
// server.
const LegacyServerID = legacyIndexName

// namespace is the on-disk subtree that owns every artifact of one tmux
// server: index, session snapshots, scrollback and locks. Nothing ever crosses
// between namespaces.
type namespace struct {
	id     string
	socket string
	root   string
}

// ServerID derives the stable, filesystem-safe namespace id of a tmux server
// socket path. The hash suffix makes distinct sockets collide-free even when
// their sanitized names agree. An empty socket denotes legacy records.
func ServerID(socketPath string) string {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return LegacyServerID
	}

	sum := sha256.Sum256([]byte(socketPath))
	suffix := hex.EncodeToString(sum[:])[:8]

	safe := strings.Trim(sanitizeName(socketPath), "_")
	if safe == "" {
		safe = "server"
	}

	if len(safe) > maxNamespaceNameLn {
		safe = strings.Trim(safe[:maxNamespaceNameLn], "_")
	}

	return fmt.Sprintf("%s-%s", safe, suffix)
}

// ServerLabel is the short human-readable marker used by list/picker output.
func ServerLabel(socketPath string) string {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return LegacyServerID
	}

	return filepath.Base(filepath.Clean(socketPath))
}

func namespaceFor(baseDir, socketPath string) namespace {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return legacyNamespace(baseDir)
	}

	socketPath = filepath.Clean(socketPath)
	id := ServerID(socketPath)

	return namespace{
		id:     id,
		socket: socketPath,
		root:   filepath.Join(baseDir, serversDirName, id),
	}
}

func legacyNamespace(baseDir string) namespace {
	return namespace{
		id:     LegacyServerID,
		socket: "",
		root:   filepath.Join(baseDir, LegacyServerID),
	}
}

// namespaceByID resolves a namespace discovered through the index. Unknown
// ids are rejected so callers can never address an arbitrary directory.
func namespaceByID(baseDir, namespaceID string) (namespace, error) {
	namespaceID = strings.TrimSpace(namespaceID)
	if namespaceID != LegacyServerID &&
		(namespaceID == "" || namespaceID == "." || namespaceID == ".." ||
			strings.ContainsAny(namespaceID, `/\`)) {
		return namespace{}, fmt.Errorf("%w: %q", errInvalidNamespace, namespaceID)
	}

	root := filepath.Join(baseDir, serversDirName, namespaceID)
	if namespaceID == LegacyServerID {
		root = filepath.Join(baseDir, LegacyServerID)
	}

	info, err := os.Stat(root)
	if err != nil {
		return namespace{}, fmt.Errorf("resolve namespace %q: %w", namespaceID, err)
	}
	if !info.IsDir() {
		return namespace{}, fmt.Errorf("%w: %q", errInvalidNamespace, namespaceID)
	}

	socket := ""
	if namespaceID != LegacyServerID {
		socket = readNamespaceSocket(root)
	}

	return namespace{id: namespaceID, socket: socket, root: root}, nil
}

var errInvalidNamespace = errors.New("invalid server namespace")

type namespaceIdentity struct {
	SocketPath string    `json:"socket_path"`
	FirstSeen  time.Time `json:"first_seen,omitzero"`
}

func readNamespaceSocket(root string) string {
	path := filepath.Join(root, identityFileName)

	file, err := os.Open(path) // #nosec G304 -- namespace root derived from validated id
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	var identity namespaceIdentity
	decoder := json.NewDecoder(io.LimitReader(file, identityReadLimit))
	decodeErr := decoder.Decode(&identity)
	if decodeErr != nil {
		return ""
	}

	return strings.TrimSpace(identity.SocketPath)
}

// writeIdentity records which tmux server owns a namespace. It is written once
// and never rewritten, so interrupted or concurrent processes cannot
// re-stamp ownership.
func (namespace namespace) writeIdentity() error {
	if namespace.socket == "" {
		return nil
	}

	path := filepath.Join(namespace.root, identityFileName)
	_, statErr := os.Stat(path)
	if statErr == nil {
		return nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat namespace identity: %w", statErr)
	}

	identity := namespaceIdentity{SocketPath: namespace.socket, FirstSeen: time.Now().UTC()}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal namespace identity: %w", err)
	}
	data = append(data, '\n')

	// Unique temp name: several savers can ensure the layout concurrently.
	tmp, err := os.CreateTemp(namespace.root, identityFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create namespace identity: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	_, err = tmp.Write(data)
	if err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write namespace identity: %w", err)
	}
	err = tmp.Chmod(defaultFilePerm)
	if err != nil {
		_ = tmp.Close()

		return fmt.Errorf("chmod namespace identity: %w", err)
	}
	err = tmp.Close()
	if err != nil {
		return fmt.Errorf("close namespace identity: %w", err)
	}

	err = os.Rename(tmpName, path)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("publish namespace identity: %w", err)
	}

	return nil
}

func (ns namespace) indexPath() string {
	return filepath.Join(ns.root, indexFileName)
}

func (ns namespace) sessionsDir() string {
	return filepath.Join(ns.root, sessionsDirName)
}

func (ns namespace) sessionPath(name string) string {
	return filepath.Join(ns.root, sessionsDirName, sanitizeName(name)+".json")
}

func (ns namespace) scrollRoot() string {
	return filepath.Join(ns.root, scrollbackDir)
}

func (ns namespace) locksDir() string {
	return filepath.Join(ns.root, sessionLocksDirName)
}

func (ns namespace) indexLockPath() string {
	return filepath.Join(ns.root, ".index.lock")
}
