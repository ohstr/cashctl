package ledger

// Store is the storage seam between the command layer and the concrete ledger
// container. It is exactly the set of *Ledger methods cmd calls — no more — so
// the command layer depends on this interface rather than *Ledger directly.
//
// This is stage 1 of the pocket-ledger migration (the plan's §8 / Phase P2): a
// pure refactor with zero behaviour change. The existing *Ledger satisfies it
// unchanged (asserted below); a later stage implements Store over internal/pocket
// and swaps it in behind a construction toggle, family by family, with the
// recovery suite green throughout.
//
// Entry, Call, Handoff, Order and Withdrawal stay concrete value/view types the
// cmd layer reads directly — only the container goes behind this interface, so a
// pocket-backed Store can return the same Entry/Call values mapped from pocket
// data without the command layer noticing.
type Store interface {
	// Held bills and their state.
	Add(e Entry) (*Entry, error)
	Find(id string) (*Entry, bool)
	FindByToken(token string) (*Entry, bool)
	Held() []Entry
	SetStatus(id, status string) error
	SetVerified(id string, verified bool) error

	// Action log + hand-offs.
	AppendHistory(action, detail string)
	AddHandoff(h Handoff)

	// Read-only views of the container's slices (cmd reads these fields
	// directly today; behind the seam they are accessor methods, named to not
	// collide with the underlying Entries/Calls/Handoffs/History fields).
	AllEntries() []Entry
	AllCalls() []Call
	AllHandoffs() []Handoff
	AllHistory() []HistoryEntry

	// In-flight calls (write-ahead / settle).
	AddCall(c Call) (*Call, error)
	FindCall(callID string) (*Call, bool)
	OpenCallFor(entryID string) (*Call, bool)
	RemoveCall(callID string) bool

	// VerifyWritten is the write-ahead durability read-back: re-read the call
	// from the store ON A FRESH READ and confirm it landed (every column +
	// secret commitments). The guarantee a bill-creating call is durable before
	// it is sent. The *Ledger reads cashctl.db; the pocket Store re-reads its
	// sealed pending_calls — both apply ledger.VerifyCallReadBack.
	VerifyWritten(want Call) error

	// Derived-id gate: the cross-process re-placement lock (NIP-CASH §Lost
	// Replies / NIP-CP §Writing derived id). InsertDerived's primary key is the
	// lock — of two processes re-placing one call, only one inserts (the other
	// gets ErrDerivedRecorded). Behind the seam today they wrap the package funcs;
	// a pocket Store keeps them in its own derived_ids.
	InsertDerived(requestID, parent string, deadline *int64, keepUntil int64) error
	RemoveDerived(requestID string) error
	DerivedParent(requestID string) (string, error)
	DerivedRecorded(requestID string) (bool, error)

	// Staged order/withdrawal bookkeeping.
	StageOrder(o Order)
	DropStagedOrders()
	StageWithdrawal(w Withdrawal)
	DropStagedWithdrawals()

	// Orders (NIP-CO) — a separate lazily-loaded funds collection, read/written on
	// demand (not part of the in-memory entry/call snapshot).
	PutOrder(o Order) error
	FindOrder(id string) (Order, bool, error)
	OrderForCall(callID string) (Order, bool, error)
	Orders() ([]Order, error)
	DeleteOrder(id string) error

	// Withdrawals (cash_withdraw).
	PutWithdrawal(w Withdrawal) error
	FindWithdrawal(id string) (Withdrawal, bool, error)
	WithdrawalForCall(callID string) (Withdrawal, bool, error)
	Withdrawals() ([]Withdrawal, error)

	// Persist.
	Save() error

	// Close releases any resources the store holds. The old *Ledger holds none
	// (no-op); the pocket-backed Store holds a bbolt single-writer lock, so a
	// command MUST Close it (defer) or the next command's open deadlocks.
	Close() error
}

// The concrete ledger satisfies the seam unchanged.
var _ Store = (*Ledger)(nil)

// Close is a no-op for the in-memory/cashctl.db ledger — it holds no lock to
// release. (The pocket-backed Store releases its bbolt file lock here.)
func (l *Ledger) Close() error { return nil }

// VerifyWritten reads the call back from cashctl.db on a fresh connection and
// applies the durability compare — the existing package-level behaviour, now
// also reachable through the Store seam so cmd can call it store-agnostically.
func (l *Ledger) VerifyWritten(want Call) error { return VerifyWritten(want) }

// AllEntries/AllCalls/AllHandoffs/AllHistory expose the container's slices as
// read-only accessors (the cmd layer reads the fields directly today; behind the
// Store seam it calls these). They return the backing slices as-is — the cmd
// layer only reads them — so this is a zero-behaviour-change wrapper.
func (l *Ledger) AllEntries() []Entry        { return l.Entries }
func (l *Ledger) AllCalls() []Call           { return l.Calls }
func (l *Ledger) AllHandoffs() []Handoff     { return l.Handoffs }
func (l *Ledger) AllHistory() []HistoryEntry { return l.History }

// Derived-id gate — the concrete ledger wraps the package-level funcs (which key
// off the shared cashctl.db derived_ids table), so routing the journal through
// the Store seam is zero-behaviour-change for the old ledger. (An unqualified
// call inside a method resolves to the package func, not the method — no
// recursion.)
func (l *Ledger) InsertDerived(requestID, parent string, deadline *int64, keepUntil int64) error {
	return InsertDerived(requestID, parent, deadline, keepUntil)
}
func (l *Ledger) RemoveDerived(requestID string) error           { return RemoveDerived(requestID) }
func (l *Ledger) DerivedParent(requestID string) (string, error) { return DerivedParent(requestID) }
func (l *Ledger) DerivedRecorded(requestID string) (bool, error) { return DerivedRecorded(requestID) }
