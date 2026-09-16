package cmd

// ptrTo returns a pointer to a copy of v — for constructing a *bool/*int64/
// etc. literal inline (ledger.Entry's own optional fields are pointers, and
// Go has no address-of-literal syntax), without a throwaway local variable
// at every call site.
func ptrTo[T any](v T) *T { return &v }
