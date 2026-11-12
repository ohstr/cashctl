// Package pocket is cashctl's local NIP-CP Cash Pocket: the canonical event
// store (pocket.db, a nmilat/relay.EventStore holding the gift-wrapped 1059
// events) and the decoded state cache (pocket-cache.db). Phase P1 builds the
// store adapter + the cache; the Journal swap and the cash-pocket commands are
// later phases.
//
// No relay, no socket: Store is the event store used as a library, in-process.
// nipCP.Relays is named for the websocket case but is only Publish/Query, which
// an in-process event store satisfies directly.
package pocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip11"
	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/relay"
)

// Store is the canonical Cash Pocket event store: the gift-wrapped 1059 events,
// persisted locally in pocket.db (a nmilat/relay.EventStore over bbolt). It
// implements nipCP.Relays so nmilat's pocket sync/resolve machinery reads and
// writes it exactly as it would a remote relay — but entirely in-process, with
// nothing listening and nothing exposed. The relay-URL argument of Publish/Query
// is ignored: there is one local store.
type Store struct {
	es *relay.EventStore
}

var _ nipCP.Relays = (*Store)(nil)

// OpenStore opens (creating if absent) the pocket event store at path.
func OpenStore(path string) (*Store, error) {
	// A zero Limitation is filled with defaults by NewEventStore (it sets
	// MaxIndexableTags before migrations); we impose no extra limits locally.
	es, err := relay.NewEventStore(path, &nip11.Limitation{})
	if err != nil {
		return nil, fmt.Errorf("pocket: open store %s: %w", path, err)
	}
	return &Store{es: es}, nil
}

// EventStore returns the underlying nmilat/relay.EventStore (pocket.db), so
// `cash pocket serve` can front it with a relay SessionHandler over a socket —
// the one place the pocket is exposed, gated by WithRecipientOnlyKinds(1059).
func (s *Store) EventStore() *relay.EventStore { return s.es }

// Close releases the store's bbolt handle. It must be called before another
// process (e.g. `cash pocket serve`) opens the same file: bbolt holds a
// single-writer file lock (see the plan's risk 11).
func (s *Store) Close() {
	if s.es != nil {
		s.es.Close()
	}
}

// Publish stores one event (nipCP.Relays). A repeated event id is a no-op — the
// store is keyed by id — so re-publishing a wrap is idempotent.
func (s *Store) Publish(ctx context.Context, _ string, ev *nip01.Event) error {
	if ev == nil {
		return fmt.Errorf("pocket: publish nil event")
	}
	if err := s.es.InsertEvents(ctx, []*nip01.Event{ev}); err != nil {
		if errors.Is(err, relay.ErrEventDuplicated) {
			return nil // already stored: re-publishing a wrap is a no-op
		}
		return fmt.Errorf("pocket: publish %s: %w", ev.ID, err)
	}
	return nil
}

// Query returns the stored events a filter matches (nipCP.Relays). The store
// applies the filter (kinds, #tags, since/until, limit); this maps each result's
// raw JSON back to a nip01.Event. Duplicate ids are collapsed, so a caller never
// sees the same event twice from one query.
func (s *Store) Query(ctx context.Context, _ string, f nip01.SubscriptionFilter) ([]*nip01.Event, error) {
	hits, err := s.es.FindEvents(ctx, &f)
	if err != nil {
		return nil, fmt.Errorf("pocket: query: %w", err)
	}
	out := make([]*nip01.Event, 0, len(hits))
	seen := make(map[string]bool, len(hits))
	for _, h := range hits {
		if h == nil || seen[h.EventID] {
			continue
		}
		seen[h.EventID] = true
		var ev nip01.Event
		if err := json.Unmarshal(h.Bytes, &ev); err != nil {
			return nil, fmt.Errorf("pocket: decode stored event %s: %w", h.EventID, err)
		}
		out = append(out, &ev)
	}
	return out, nil
}
