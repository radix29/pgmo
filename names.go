package pgmo

// ============================================================
// Name matching
// ============================================================
//
// A stored PostgreSQL name is case-sensitive: the server folds only what is
// typed unquoted, at parse time, so Orders bare means orders while "Orders"
// and orders are two objects side by side in one schema. pgmo therefore
// matches names exactly, everywhere — in SQL by `=` on the name column and in
// Go by matchName — and never falls back to a case-insensitive match. gosmo's
// matchName folds case when no exact match exists, because SQL Server names
// compare under the instance's (usually case-insensitive) collation; that
// fallback here would answer a lookup of "orders" with the object "Orders"
// while orders itself may be created a moment later, and could not tell the
// two apart once both exist.
//
// A name a person types is turned into the stored name before the lookup,
// the server's way: pgsql.FoldIdent (quoted → unquoted, bare → ASCII
// lowered). That is a caller's step, since only the caller knows whether the
// text was typed or came from the catalog.
//
// TestNoCaseFoldingOfNames refuses Go case folding (strings.EqualFold,
// ToLower, …) and SQL case folding (lower(), upper(), ILIKE, ~*) in the
// package, except where caseFoldingAllowed says why the text is not a name.

// matchName returns the one item whose key is exactly name. None is
// ErrNotFound and more than one is ErrAmbiguous — returned bare, for the
// caller to wrap naming the lookup ("publication %s: %w"). There is no
// case-insensitive fallback (see above).
func matchName[T any](items []T, name string, key func(T) string) (T, error) {
	var found, zero T
	n := 0
	for _, it := range items {
		if key(it) == name {
			found = it
			n++
		}
	}
	switch n {
	case 0:
		return zero, ErrNotFound
	case 1:
		return found, nil
	}
	return zero, ErrAmbiguous
}
