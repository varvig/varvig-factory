package varvigcli

// Two repository kinds, and why the difference is in the type system.
//
// A factory holds one **coordination** repository and N **project**
// repositories, and a cell holds a full replica of the coordination repo plus a
// replica of each project it works on. The split is about authority, not
// convenience: without it, envelopes and leases are duplicated into every
// project repo and there is no single authoritative answer to "what may this
// cell spend". One factory repo, N project repos, one place to look.
//
// # Why these are distinct types and not two values of one interface
//
// Both kinds are reached through the same Varvig surface, so nothing stops a
// caller passing either where either is wanted — and the one mistake that
// matters here is resolved by spending money. Writing a lease into a project
// repo would fragment authority silently: every project would carry its own
// copy of what a cell may spend, each of them plausible, and the sum would
// exceed the envelope with nothing in the system able to notice.
//
// So the kinds are separate types and the functions that touch them name which
// kind they want. Handing a project replica to something that writes a lease is
// a compile error, which is the only kind of guarantee worth having about a
// mistake nobody would catch by reading.
//
// # The single-project case
//
// One repository can legitimately play both roles: a factory with a single
// codebase has nothing to fragment, and Collapsed builds that configuration in
// one call. The types stay distinct even then — the roles are still two, and a
// call site that says which one it means keeps saying so if the deployment later
// grows a second project.

// FactoryRepo is a replica of the factory-coordination repository: who the
// factory is and what it may spend.
//
// It holds membership and allowed_keys, cell capability objects, interface
// schemas, overseer envelopes, and per-cell budget leases. Its scope is the
// factory, not any codebase, and a cell replicates exactly one.
type FactoryRepo struct{ Varvig }

// ProjectRepo is a replica of one project repository: the work itself.
//
// It holds tickets and intents, claims, attempts, evidence and environment
// descriptors, artifact-ref objects, and the reservations recording effectful
// actions taken against that codebase. A cell replicates whichever projects it
// works on, and only those.
type ProjectRepo struct{ Varvig }

// Collapsed returns both handles over one repository, for a factory with a
// single project.
//
// It exists so the degenerate case is one obvious call rather than two wraps a
// reader has to compare, and so the fact that both roles are being served by one
// replica is stated rather than inferred.
func Collapsed(v Varvig) (FactoryRepo, ProjectRepo) {
	return FactoryRepo{v}, ProjectRepo{v}
}
