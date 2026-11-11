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

	// Staged order/withdrawal bookkeeping.
	StageOrder(o Order)
	DropStagedOrders()
	StageWithdrawal(w Withdrawal)
	DropStagedWithdrawals()

	// Persist.
	Save() error
}

// The concrete ledger satisfies the seam unchanged.
var _ Store = (*Ledger)(nil)

// AllEntries/AllCalls/AllHandoffs/AllHistory expose the container's slices as
// read-only accessors (the cmd layer reads the fields directly today; behind the
// Store seam it calls these). They return the backing slices as-is — the cmd
// layer only reads them — so this is a zero-behaviour-change wrapper.
func (l *Ledger) AllEntries() []Entry         { return l.Entries }
func (l *Ledger) AllCalls() []Call            { return l.Calls }
func (l *Ledger) AllHandoffs() []Handoff      { return l.Handoffs }
func (l *Ledger) AllHistory() []HistoryEntry  { return l.History }
