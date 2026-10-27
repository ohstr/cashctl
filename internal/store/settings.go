package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Settings are user settings kept in the meta table under "setting:<key>".

// Setting reads a setting; ok is false when it is not set.
func Setting(key string) (value string, ok bool, err error) {
	db, err := Open()
	if err != nil {
		return "", false, err
	}
	defer func() { _ = db.Close() }()
	err = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, "setting:"+key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cashctl.db: reading setting %s: %w", key, err)
	}
	return value, true, nil
}

// SetSetting writes a setting; an empty value removes it.
func SetSetting(key, value string) error {
	return WithBusyRetry(func() error {
		db, err := Open()
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		if value == "" {
			_, err = db.Exec(`DELETE FROM meta WHERE key = ?`, "setting:"+key)
		} else {
			_, err = db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, "setting:"+key, value)
		}
		if err != nil {
			return fmt.Errorf("cashctl.db: writing setting %s: %w", key, err)
		}
		return nil
	})
}
