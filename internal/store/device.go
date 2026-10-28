package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The device record says which copy of the wallet placed a call (NIP-CASH
// §Lost Replies: only the device that placed a call re-places it). A wallet
// file copied or restored elsewhere is a different device: its fingerprint no
// longer matches, so it gets a new id and every call placed before becomes
// foreign to it — looked up, never placed again from the copy.

// deviceRotatedNote is the history line written when the id rotates.
const deviceRotatedNote = "this wallet file was copied or restored, so it now has a new device id: calls placed from the old copy are looked up from here, never placed again"

// ensureDevice creates the device record on first open, and rotates its id
// when the file's fingerprint changed. Runs inside initSchema's transaction.
func ensureDevice(ctx context.Context, conn *sql.Conn, path string) error {
	fp, err := fingerprint(path)
	if err != nil {
		return fmt.Errorf("cashctl.db: device fingerprint: %w", err)
	}
	var stored string
	err = conn.QueryRowContext(ctx, `SELECT fingerprint FROM device WHERE id = 1`).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err := newDeviceID()
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO device (id, device_id, fingerprint, created_at) VALUES (1, ?, ?, ?)`,
			id, fp, time.Now().Unix())
		return err
	case err != nil:
		return fmt.Errorf("cashctl.db: reading the device record: %w", err)
	case stored == fp:
		return nil
	}
	id, err := newDeviceID()
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE device SET device_id = ?, fingerprint = ? WHERE id = 1`, id, fp); err != nil {
		return fmt.Errorf("cashctl.db: rotating the device id: %w", err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO history (at, action, detail) VALUES (?, 'device', ?)`,
		time.Now().UTC().Format(time.RFC3339), deviceRotatedNote)
	return err
}

// DeviceID is this wallet file's device id: 64 lowercase hex.
func DeviceID() (string, error) {
	db, err := Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var id string
	if err := db.QueryRow(`SELECT device_id FROM device WHERE id = 1`).Scan(&id); err != nil {
		return "", fmt.Errorf("cashctl.db: reading the device id: %w", err)
	}
	return id, nil
}

func newDeviceID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("cashctl.db: no randomness for a device id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// fingerprint is hex sha256(machine id ‖ resolved path ‖ inode): it changes
// when the file is copied, moved to another machine or restored, and not when
// the same file is reached through another spelling of its path (a symlinked
// $HOME or XDG_CONFIG_HOME). The device number is left out: it can change
// across container restarts with the file untouched. Where the platform gives
// no inode, the hostname stands in for it.
func fingerprint(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	_, ino, err := fileIDFn(abs)
	if err != nil {
		return "", err
	}
	file := fmt.Sprint(ino)
	if ino == 0 {
		host, _ := os.Hostname()
		file = "host:" + host
	}
	h := sha256.New()
	for _, part := range []string{machineIDFn(), abs, file} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// machineID names this machine: the systemd or D-Bus machine id where there
// is one, else nothing. Never the hostname, which changes with the network or
// each container run while the file stays where it was.
func machineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id
			}
		}
	}
	return ""
}

// machineIDFn and fileIDFn are swapped in tests.
var (
	machineIDFn = machineID
	fileIDFn    = fileID
)
