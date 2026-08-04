package shenmodel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatternMatchingAndRecursion(t *testing.T) {
	program := MustParse(`
(define member?
  _ [] -> false
  X [X | _] -> true
  X [_ | Rest] -> (member? X Rest))
`)
	value, err := program.Call("member?", "b", List{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if value != true {
		t.Fatalf("member? = %#v", value)
	}
}

func TestLetBindsLexicalVariable(t *testing.T) {
	program := MustParse(`
(define choose
  X -> (let Y (+ X 1) (= Y 2)))
`)
	value, err := program.Call("choose", uint64(1))
	if err != nil {
		t.Fatal(err)
	}
	if value != true {
		t.Fatalf("choose = %#v", value)
	}
}

func TestMuxSemanticMutationChangesExecutableBehavior(t *testing.T) {
	specPath := filepath.Join("..", "..", "specs", "mux.shen")
	sourceBytes, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	original, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	// Session fields: clients, seq, snapshot, dimensions, lock, exited, and
	// nested control state: controller and pending attach are independent lists.
	session := List{List{"client-a"}, uint64(0), "opaque-snapshot", List{uint64(80), uint64(24)}, false, false, List{List{"client-a"}, List{}}}
	got, err := original.Call("mux.accept-input?", session, "client-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != true {
		t.Fatalf("original accept-input? = %#v", got)
	}

	needle := `(define mux.accept-input?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked Exited Control] C ->
    (and (not Locked)
         (and (not Exited)
              (and (mux.member? C Clients)
                   (= (mux.controller Locked Control) [C])))))`
	replacement := `(define mux.accept-input?
  {session --> client-id --> boolean}
  _ _ -> false)`
	mutatedSource := strings.Replace(source, needle, replacement, 1)
	if mutatedSource == source {
		t.Fatal("test mutation did not match spec")
	}
	mutated, err := Parse(mutatedSource)
	if err != nil {
		t.Fatal(err)
	}
	got, err = mutated.Call("mux.accept-input?", session, "client-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != false {
		t.Fatalf("mutated accept-input? = %#v", got)
	}
}

func TestMuxConstructorPredicateMutationChangesExecutableBehavior(t *testing.T) {
	specPath := filepath.Join("..", "..", "specs", "mux.shen")
	sourceBytes, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	original, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := original.Call("mux.valid-client-id?", "client-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != true {
		t.Fatalf("original valid-client-id? = %#v", got)
	}

	needle := `(define mux.valid-client-id?
  {string --> boolean}
  Id -> (not (= Id "")))`
	replacement := `(define mux.valid-client-id?
  {string --> boolean}
  _ -> false)`
	mutatedSource := strings.Replace(source, needle, replacement, 1)
	if mutatedSource == source {
		t.Fatal("constructor predicate mutation did not match spec")
	}
	mutated, err := Parse(mutatedSource)
	if err != nil {
		t.Fatal(err)
	}
	got, err = mutated.Call("mux.valid-client-id?", "client-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != false {
		t.Fatalf("mutated valid-client-id? = %#v", got)
	}
}
