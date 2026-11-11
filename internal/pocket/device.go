package pocket

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/ohstr/nmilat/nipCP"
)

// Cache also holds the LOCAL_ONLY device bookkeeping NIP-CP needs: the device id
// and the derived-ids this device has placed (so a retried call is never placed
// twice). These are precious, not rebuildable — losing them is non-fatal but
// degrades (see the plan's risk 19) — so they live alongside the decoded cache,
// never in the synced event store.
var _ nipCP.DeviceStore = (*Cache)(nil)

// DeviceID returns this device's id, creating and persisting one on first use.
func (c *Cache) DeviceID(_ context.Context) (string, error) {
	id, err := c.metaGet("device_id")
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	id, err = nipCP.NewDeviceID()
	if err != nil {
		return "", err
	}
	if err := c.metaSet("device_id", id); err != nil {
		return "", err
	}
	return id, nil
}

// RecordDerived notes a derived id placed here, kept until keepUntil.
func (c *Cache) RecordDerived(_ context.Context, id string, keepUntil int64) error {
	_, err := c.db.Exec(
		`INSERT INTO derived_ids(id, keep_until) VALUES(?, ?)
		 ON CONFLICT(id) DO UPDATE SET keep_until = MAX(derived_ids.keep_until, excluded.keep_until)`,
		id, keepUntil)
	if err != nil {
		return fmt.Errorf("pocket: record derived id: %w", err)
	}
	return nil
}

// DerivedPlaced reports whether a derived id was already placed here.
func (c *Cache) DerivedPlaced(_ context.Context, id string) (bool, error) {
	var one int
	err := c.db.QueryRow(`SELECT 1 FROM derived_ids WHERE id = ?`, id).Scan(&one)
	switch err {
	case sql.ErrNoRows:
		return false, nil
	case nil:
		return true, nil
	default:
		return false, fmt.Errorf("pocket: derived placed: %w", err)
	}
}

// Cursor is the sync high-water mark (the newest wrap created_at seen). A sync
// pulls from Cursor() minus the NIP-59 2-day window (nipCP.IncrementalFilter)
// so a back-dated wrap is never missed — see the plan's risk 13.
func (c *Cache) Cursor() (int64, error) {
	v, err := c.metaGet("cursor")
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// SetCursor advances the high-water mark; it never moves backwards.
func (c *Cache) SetCursor(v int64) error {
	cur, err := c.Cursor()
	if err != nil {
		return err
	}
	if v <= cur {
		return nil
	}
	return c.metaSet("cursor", strconv.FormatInt(v, 10))
}

func (c *Cache) metaGet(key string) (string, error) {
	var v string
	err := c.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	switch err {
	case sql.ErrNoRows:
		return "", nil
	case nil:
		return v, nil
	default:
		return "", fmt.Errorf("pocket: meta get %s: %w", key, err)
	}
}

func (c *Cache) metaSet(key, value string) error {
	_, err := c.db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("pocket: meta set %s: %w", key, err)
	}
	return nil
}
