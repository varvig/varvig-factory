// Package guard holds no code — only build-failing guards that keep Factory
// from growing the two things it must not have.
//
// Both are stated as prohibitions in the spec, and both have an
// attractive-looking violation, which is exactly why a reviewer's vigilance is
// the wrong mechanism. A guard test turns "we agreed not to do that" into a red
// CI run on the commit that does it.
package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	// This file is <root>/guard/guard_test.go.
	return filepath.Join(filepath.Dir(file), "..")
}

// goFiles walks the module's non-test Go source. Test files are excluded: a test
// may legitimately name a forbidden concept in order to assert its absence, and
// this very file is the proof.
func goFiles(t *testing.T, root string, skipDirs ...string) []string {
	t.Helper()
	skip := map[string]bool{".git": true, "guard": true}
	for _, d := range skipDirs {
		skip[d] = true
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			if skip[rel] || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("guard: found no source files to scan; the walk is broken, not the code")
	}
	return out
}

// schedulerIdentifiers are the names a second scheduler would have to introduce.
//
// FACTORY.md §9.9: "assert Factory never assigns read/write sets itself; it
// submits and lets varvig serialize." A textual guard cannot prove the negative,
// but it can catch the shape: computing an affected set, deriving a write set,
// ordering work, resolving a conflict. Every one of these is a function varvig
// already has, and a Factory function with one of these names is a
// reimplementation of it (§1).
//
// The list names *actions*, not nouns. "writeSet" appears legitimately all over
// this module — the loop reads a ticket's declared write set and honours it — so
// forbidding the noun would forbid the correct behaviour. What must not exist is
// a Factory function that *computes* one.
var schedulerIdentifiers = []string{
	"computeWriteSet", "deriveWriteSet", "assignWriteSet", "buildWriteSet",
	"computeReadSet", "deriveReadSet", "assignReadSet",
	"affectedSet", "computeAffected", "resolveAffected",
	"serializeTasks", "orderTasks", "scheduleTasks", "planTasks",
	"resolveConflict", "detectConflict", "conflictSet",
	"admissionOrder", "rankTickets", "topologicalOrder",
}

// TestNoSecondScheduler is FACTORY.md §9.9.
//
// It reads declarations rather than grepping text, so a mention inside a comment
// or a string — an explanation of why Factory does not do this, for instance —
// does not trip it. What trips it is a declaration: a function, method, type or
// variable that exists to compute scheduling.
func TestNoSecondScheduler(t *testing.T) {
	root := moduleRoot(t)
	forbidden := map[string]bool{}
	for _, id := range schedulerIdentifiers {
		forbidden[strings.ToLower(id)] = true
	}

	var violations []string
	fset := token.NewFileSet()
	for _, path := range goFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var name string
			switch decl := n.(type) {
			case *ast.FuncDecl:
				name = decl.Name.Name
			case *ast.TypeSpec:
				name = decl.Name.Name
			case *ast.ValueSpec:
				for _, id := range decl.Names {
					if forbidden[strings.ToLower(id.Name)] {
						violations = append(violations, relPath(root, path)+": "+id.Name)
					}
				}
				return true
			default:
				return true
			}
			if forbidden[strings.ToLower(name)] {
				violations = append(violations, relPath(root, path)+": "+name)
			}
			return true
		})
	}
	if len(violations) > 0 {
		t.Fatalf(`Factory has grown a second scheduler (FACTORY.md §1, §9.9).

These declarations compute scheduling, which is varvig's job:
  %s

Factory decides whether this cell should attempt this ticket — that is claim
policy. varvig's scheduler decides how concurrent work inside a cell interleaves:
read/write sets, serialization, regeneration on CAS failure. Call `+"`varvig task`"+`
and let varvig handle concurrency.`, strings.Join(violations, "\n  "))
	}
}

// TestFactorySubmitsAndDoesNotSerialize is the behavioural half of §9.9.
//
// The guard above catches a function named like a scheduler. This one catches the
// same mistake wearing a different name, by checking the direction of the
// dependency: everything the loop knows about a ticket's scope arrives through
// the varvig client, so the *only* place a write set can come from is a read.
func TestFactorySubmitsAndDoesNotSerialize(t *testing.T) {
	root := moduleRoot(t)
	// varvigcli.Scope is the type carrying a declared read/write set. Any
	// package that constructs one outside of varvigcli (and outside tests) is
	// deciding a scope rather than reading it.
	//
	// Two exemptions, both for code that plays the *author* rather than the cell:
	// varvigcli itself, which parses a scope out of varvig's output, and
	// cmd/factory-demo, which seeds a repository with tickets — work a human does
	// with `varvig tickets scope` and which has to come from somewhere for a demo
	// to have anything to run against.
	var violations []string
	fset := token.NewFileSet()
	for _, path := range goFiles(t, root, "varvigcli") {
		if strings.HasPrefix(relPath(root, path), "cmd/factory-demo/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Scope" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "varvigcli" {
				return true
			}
			// An empty literal is a zero value — a declared-nothing scope, which
			// is how "unschedulable" is represented. Populating one is the
			// violation.
			if len(lit.Elts) > 0 {
				violations = append(violations, relPath(root, path))
			}
			return true
		})
	}
	if len(violations) > 0 {
		t.Fatalf(`Factory constructs a ticket scope instead of reading varvig's (§1, §9.9):
  %s
A read/write set is declared on the ticket and derived by varvig. Factory reads
it and honours it; it never decides it.`, strings.Join(violations, "\n  "))
	}
}

// TestNoThirdPartyDependencies keeps the module dependency-free.
//
// The README promises it, varvig's own design forbids a Factory-layer package
// pulling a vendor SDK into the seam it exists to hide, and — the practical
// reason — a single static binary per platform is much easier to keep honest with
// nothing but the standard library in it. A promise nothing checks is a promise
// that drifts. CI checks this too; having it here means it fails during
// development rather than at review.
func TestNoThirdPartyDependencies(t *testing.T) {
	root := moduleRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "require") {
		t.Fatalf("go.mod has a require directive; this module must stay dependency-free:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); err == nil {
		t.Fatal("go.sum exists, so something outside the standard library was added")
	}
}

// TestNoTierBranching is FACTORY.md §1.2: tiers are configuration profiles, not
// code paths. A comparison against a tier or profile name outside the package
// that defines the templates is a branch on tier, and "if a tier ever requires a
// branch in the code, the abstraction has failed".
func TestNoTierBranching(t *testing.T) {
	root := moduleRoot(t)
	tierNames := map[string]bool{`"micro"`: true, `"mini"`: true, `"medium"`: true}

	var violations []string
	fset := token.NewFileSet()
	for _, path := range goFiles(t, root) {
		rel := relPath(root, path)
		// profile/ defines the templates and its Template() maps a name to one;
		// cmd/ passes a name through to it. Both are naming a profile, not
		// branching on a tier inside the machinery.
		if strings.HasPrefix(rel, "profile/") || strings.HasPrefix(rel, "cmd/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{bin.X, bin.Y} {
				lit, ok := side.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && tierNames[lit.Value] {
					violations = append(violations, rel+": compares against "+lit.Value)
				}
			}
			return true
		})
	}
	if len(violations) > 0 {
		t.Fatalf(`Factory branches on a tier name (FACTORY.md §1.2):
  %s
Micro and Mini must differ only in which model runtime and budget the config
names. A branch on tier means the abstraction has failed.`, strings.Join(violations, "\n  "))
	}
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
